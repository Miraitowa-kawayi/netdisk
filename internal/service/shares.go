package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/Miraitowa-kawayi/netdisk/internal/model"
	"github.com/Miraitowa-kawayi/netdisk/internal/repository"
	"github.com/google/uuid"
)

// Shares 是分享链接的业务层。
//
// 两条路径泾渭分明：
//   - 管理路径（Create / List / Revoke）走 owner 身份；
//   - 访问路径（Resolve / OpenDownload）**只认 token、没有登录态** ——
//     所以"目标还在不在、是不是在这棵分享子树里"必须在这里自己校验干净，
//     不能指望上游的鉴权中间件。
type Shares struct {
	store  *repository.Store
	files  *Files // 复用文件下载（取 blob + storage.Open），不重抄一遍
	logger *slog.Logger
}

func NewShares(store *repository.Store, files *Files, logger *slog.Logger) *Shares {
	return &Shares{store: store, files: files, logger: logger}
}

// ShareInput 是建一条分享的请求。
type ShareInput struct {
	OwnerID   uuid.UUID
	NodeID    uuid.UUID
	ExpiresIn time.Duration // 0 = 永不过期
}

// ShareView 是管理接口回给客户端的一条分享。
type ShareView struct {
	Share model.Share `json:"share"`
	URL   string      `json:"url"` // 相对路径，客户端自己拼 host
}

// Create 给一个节点建分享链接。
//
// 只允许分享"活着的"节点：自己的、没被弱删、祖先链上也干净 —— 否则会造出一条一出生就 404 的链接。
func (s *Shares) Create(ctx context.Context, in ShareInput) (ShareView, error) {
	live, err := s.store.NodeIsLive(ctx, in.OwnerID, in.NodeID)
	if err != nil {
		return ShareView{}, err
	}
	if !live {
		return ShareView{}, ErrNotFound
	}

	token, err := newShareToken()
	if err != nil {
		return ShareView{}, err
	}
	var expiresAt *time.Time
	if in.ExpiresIn > 0 {
		at := time.Now().Add(in.ExpiresIn)
		expiresAt = &at
	}

	share, err := s.store.CreateShare(ctx, in.NodeID, in.OwnerID, token, expiresAt)
	if err != nil {
		return ShareView{}, err
	}
	return ShareView{Share: share, URL: "/api/v1/share/" + token}, nil
}

// List 列我建过的分享。
func (s *Shares) List(ctx context.Context, ownerID uuid.UUID) ([]model.Share, error) {
	return s.store.ListSharesByOwner(ctx, ownerID)
}

// Revoke 撤销一条分享。不是自己的 → ErrNotFound。
func (s *Shares) Revoke(ctx context.Context, ownerID, shareID uuid.UUID) error {
	return mapNotFound(s.store.DeleteShare(ctx, ownerID, shareID))
}

// PublicView 是匿名访问拿到的内容：节点元信息 +（目录时）直接子项。
type PublicView struct {
	Node     model.Node   `json:"node"`
	Children []model.Node `json:"children"`
}

// Resolve 匿名打开一条分享。目标是目录时连直接子项一起返回。
//
// 每次访问都重新校验 —— 这是 docs/schema.md 第 6 节那条约定的落点：
// 弱删过的目标对访问者表现成 404，而不是"文件没了、链接还能下"。
func (s *Shares) Resolve(ctx context.Context, token string) (PublicView, error) {
	share, err := s.liveShare(ctx, token)
	if err != nil {
		return PublicView{}, err
	}

	node, err := s.store.GetNode(ctx, share.CreatedBy, share.NodeID)
	if err != nil {
		return PublicView{}, mapNotFound(err)
	}

	children := make([]model.Node, 0)
	if node.IsDir {
		if children, err = s.store.ListChildren(ctx, share.CreatedBy, &node.ID); err != nil {
			return PublicView{}, err
		}
	}

	// 访问计数只做展示，写失败不该让这次访问失败。
	if err := s.store.IncrementShareVisit(ctx, share.ID); err != nil {
		s.logger.Warn("increment share visit", "share_id", share.ID, "error", err)
	}
	return PublicView{Node: node, Children: children}, nil
}

// OpenDownload 匿名下载。nodeID 为 nil 表示"就下分享的那个节点"（文件分享的常见情形）；
// 分享的是目录时，客户端带上子项 id，这里校验它确实在这棵子树里。
func (s *Shares) OpenDownload(ctx context.Context, token string, nodeID *uuid.UUID) (Download, error) {
	share, err := s.liveShare(ctx, token)
	if err != nil {
		return Download{}, err
	}

	target := share.NodeID
	if nodeID != nil && *nodeID != share.NodeID {
		ok, err := s.store.IsSelfOrDescendant(ctx, share.CreatedBy, *nodeID, share.NodeID)
		if err != nil {
			return Download{}, err
		}
		if !ok {
			// 不在这棵子树里（或链路上有节点被删）→ 一律当成"没有这个东西"。
			// 不区分"不存在"与"不属于这条分享"，免得有人拿链接当探针枚举别人的 node id。
			return Download{}, ErrNotFound
		}
		target = *nodeID
	}

	return s.files.OpenDownload(ctx, share.CreatedBy, target)
}

// liveShare 取一条可用的分享：不存在、已过期、目标已经不在（弱删/祖先被删）→ 都是 ErrNotFound。
func (s *Shares) liveShare(ctx context.Context, token string) (model.Share, error) {
	share, err := s.store.GetShareByToken(ctx, token)
	if err != nil {
		return model.Share{}, mapNotFound(err)
	}
	if share.ExpiresAt != nil && !share.ExpiresAt.After(time.Now()) {
		return model.Share{}, ErrNotFound
	}
	live, err := s.store.NodeIsLive(ctx, share.CreatedBy, share.NodeID)
	if err != nil {
		return model.Share{}, err
	}
	if !live {
		return model.Share{}, ErrNotFound
	}
	return share, nil
}

// newShareToken 生成公开访问用的随机串：128 位随机、十六进制 —— URL 安全、不可枚举。
func newShareToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

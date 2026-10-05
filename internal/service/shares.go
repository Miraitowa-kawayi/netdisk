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
// 匿名访问路径只认 token，不做登录态校验。
type Shares struct {
	store  *repository.Store
	files  *Files // 复用文件下载逻辑
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

// Create 给一个节点建分享链接；节点必须存活（属于自己、未被软删、祖先链干净）。
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

// Revoke 撤销一条分享；不是自己的返回 ErrNotFound。
func (s *Shares) Revoke(ctx context.Context, ownerID, shareID uuid.UUID) error {
	return mapNotFound(s.store.DeleteShare(ctx, ownerID, shareID))
}

// PublicView 是匿名访问拿到的内容：节点元信息 +（目录时）直接子项。
type PublicView struct {
	Node     model.Node   `json:"node"`
	Children []model.Node `json:"children"`
}

// Resolve 匿名打开一条分享，目标是目录时连直接子项一起返回。
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

	// 计数写失败不影响本次访问。
	if err := s.store.IncrementShareVisit(ctx, share.ID); err != nil {
		s.logger.Warn("increment share visit", "share_id", share.ID, "error", err)
	}
	return PublicView{Node: node, Children: children}, nil
}

// OpenDownload 匿名下载。nodeID 为 nil 表示下载分享的节点本身；
// 分享目录时客户端传子项 id，该子项必须位于这棵子树内。
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
			// 不在该子树内一律按不存在处理，避免拿链接枚举 node id。
			return Download{}, ErrNotFound
		}
		target = *nodeID
	}

	return s.files.OpenDownload(ctx, share.CreatedBy, target)
}

// liveShare 取一条可用的分享；不存在、已过期或目标已失效都返回 ErrNotFound。
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

// newShareToken 生成 128 位十六进制的公开访问 token。
func newShareToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

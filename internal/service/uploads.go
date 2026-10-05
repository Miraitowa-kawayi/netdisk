package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/Miraitowa-kawayi/netdisk/internal/model"
	"github.com/Miraitowa-kawayi/netdisk/internal/repository"
	"github.com/Miraitowa-kawayi/netdisk/internal/storage"
	"github.com/google/uuid"
)

// Uploads 是分片上传（断点续传）的业务层，会话状态为 pending/completed/aborted。
type Uploads struct {
	store   *repository.Store
	storage storage.Storage
	backend string // 写进 blobs.backend，和 Files 一致
	logger  *slog.Logger
}

func NewUploads(store *repository.Store, st storage.Storage, backend string, logger *slog.Logger) *Uploads {
	return &Uploads{store: store, storage: st, backend: backend, logger: logger}
}

// partKey 返回分片对象在存储中的 key。
func partKey(sessionID uuid.UUID, partNo int) string {
	return fmt.Sprintf("uploads/%s/%d", sessionID, partNo)
}

// checkParentDir 确认父目录存在、属于自己且是目录；parentID 为 nil 表示根目录。
func checkParentDir(ctx context.Context, store *repository.Store, ownerID uuid.UUID, parentID *uuid.UUID) error {
	if parentID == nil {
		return nil
	}
	parent, err := store.GetNode(ctx, ownerID, *parentID)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !parent.IsDir {
		return invalid("parent_id 指向的不是文件夹")
	}
	return nil
}

// mapNotFound 把 repository 的 ErrNotFound 翻成业务层的 ErrNotFound，其余原样。
func mapNotFound(err error) error {
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// CreateInput 是开一次分片上传的请求。
type CreateInput struct {
	OwnerID     uuid.UUID
	ParentID    *uuid.UUID // nil = 根目录
	Name        string
	TotalSize   int64
	ChunkSize   int64
	ContentHash *string // 客户端预申报的整份 SHA-256（可选）
}

// Create 开一个分片会话，part_count 由 total_size/chunk_size 向上取整（至少 1）。
func (u *Uploads) Create(ctx context.Context, in CreateInput) (model.UploadSession, error) {
	name, err := cleanName(in.Name)
	if err != nil {
		return model.UploadSession{}, err
	}
	if in.TotalSize < 0 {
		return model.UploadSession{}, invalid("total_size 不能为负")
	}
	if in.ChunkSize <= 0 {
		return model.UploadSession{}, invalid("chunk_size 必须为正")
	}
	if err := checkParentDir(ctx, u.store, in.OwnerID, in.ParentID); err != nil {
		return model.UploadSession{}, err
	}

	partCount := int((in.TotalSize + in.ChunkSize - 1) / in.ChunkSize)
	if partCount < 1 {
		partCount = 1 // 空文件也是"1 片、0 字节"
	}
	return u.store.CreateUploadSession(ctx, in.OwnerID, in.ParentID, name,
		in.TotalSize, in.ChunkSize, partCount, in.ContentHash)
}

// UploadStatus 是一次查询会话的结果：收到的分片号 + 还缺的分片号。
type UploadStatus struct {
	Session       model.UploadSession
	ReceivedParts []int
	MissingParts  []int
}

// Status 查会话进度，返回已收到的分片号和仍缺的分片号。
func (u *Uploads) Status(ctx context.Context, ownerID, sessionID uuid.UUID) (UploadStatus, error) {
	sess, err := u.store.GetUploadSession(ctx, ownerID, sessionID)
	if err != nil {
		return UploadStatus{}, mapNotFound(err)
	}
	parts, err := u.store.ListUploadParts(ctx, sessionID)
	if err != nil {
		return UploadStatus{}, err
	}

	have := make(map[int]bool, len(parts))
	received := make([]int, 0, len(parts))
	for _, p := range parts {
		have[p.PartNo] = true
		received = append(received, p.PartNo)
	}
	missing := make([]int, 0)
	for n := 0; n < sess.PartCount; n++ {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	return UploadStatus{Session: sess, ReceivedParts: received, MissingParts: missing}, nil
}

// List 列某用户所有 pending 会话。
func (u *Uploads) List(ctx context.Context, ownerID uuid.UUID) ([]model.UploadSession, error) {
	return u.store.ListPendingUploadSessions(ctx, ownerID)
}

// expectedPartSize 返回第 partNo 片应有的字节数；最后一片为 total_size 减去前面各片之和。
func expectedPartSize(sess model.UploadSession, partNo int) int64 {
	if partNo == sess.PartCount-1 {
		return sess.TotalSize - int64(sess.PartCount-1)*sess.ChunkSize
	}
	return sess.ChunkSize
}

// PutPart 收一个分片：流式写入存储并登记行（重复上传同一片为幂等覆盖）。
// 分片大小必须严格等于应有字节数，不符则删掉刚写入的对象并返回错误。
func (u *Uploads) PutPart(ctx context.Context, ownerID, sessionID uuid.UUID, partNo int, body io.Reader) (model.UploadPart, error) {
	sess, err := u.store.GetUploadSession(ctx, ownerID, sessionID)
	if err != nil {
		return model.UploadPart{}, mapNotFound(err)
	}
	if sess.Status != "pending" {
		return model.UploadPart{}, ErrUploadNotPending
	}
	if partNo < 0 || partNo >= sess.PartCount {
		return model.UploadPart{}, invalid("part_no 超出范围：本会话共 %d 片（0..%d）", sess.PartCount, sess.PartCount-1)
	}

	want := expectedPartSize(sess, partNo)
	key := partKey(sessionID, partNo)
	// 多读 1 字节用于判断超长，实际大小由随后的 Stat 校验。
	checksum, err := u.storage.Put(ctx, key, io.LimitReader(body, want+1), want)
	if err != nil {
		return model.UploadPart{}, fmt.Errorf("write part: %w", err)
	}
	info, err := u.storage.Stat(ctx, key)
	if err != nil {
		return model.UploadPart{}, err
	}
	if info.Size != want {
		_ = u.storage.Delete(ctx, key)
		return model.UploadPart{}, invalid("第 %d 片大小应为 %d 字节，实收 %d", partNo, want, info.Size)
	}

	if err := u.store.PutUploadPart(ctx, sessionID, partNo, info.Size, checksum); err != nil {
		_ = u.storage.Delete(ctx, key)
		return model.UploadPart{}, err
	}
	return model.UploadPart{SessionID: sessionID, PartNo: partNo, Size: info.Size, Checksum: checksum}, nil
}

// Abort 放弃一个会话：标记 aborted 并删掉已收到的分片对象；
// 已不在 pending 的会话重复放弃也返回 nil。
func (u *Uploads) Abort(ctx context.Context, ownerID, sessionID uuid.UUID) error {
	sess, err := u.store.GetUploadSession(ctx, ownerID, sessionID)
	if err != nil {
		return mapNotFound(err)
	}
	parts, err := u.store.ListUploadParts(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, p := range parts {
		if err := u.storage.Delete(ctx, partKey(sessionID, p.PartNo)); err != nil && !errors.Is(err, storage.ErrNotFound) {
			u.logger.Warn("delete aborted part", "session_id", sessionID, "part_no", p.PartNo, "error", err)
		}
	}
	if sess.Status == "pending" {
		if _, err := u.store.TransitionUploadSession(ctx, ownerID, sessionID, "pending", "aborted"); err != nil {
			return err
		}
	}
	return nil
}

// Complete 收尾一个分片会话：按 part_no 顺序拼接分片、登记 blob、建节点，
// 并把会话标记为 completed、删掉临时分片对象。
// 缺分片时返回 ErrUploadIncomplete 且不改动任何状态。
func (u *Uploads) Complete(ctx context.Context, ownerID, sessionID uuid.UUID) (model.Node, error) {
	session, err := u.store.GetUploadSession(ctx, ownerID, sessionID)
	if err != nil {
		return model.Node{}, mapNotFound(err)
	}
	if session.Status != "pending" {
		return model.Node{}, ErrUploadNotPending
	}
	parts, err := u.store.ListUploadParts(ctx, sessionID)
	if err != nil {
		return model.Node{}, err
	}
	if len(parts) != session.PartCount {
		return model.Node{}, ErrUploadIncomplete
	}
	srcKeys := make([]string, 0, len(parts))
	for _, part := range parts {
		srcKeys = append(srcKeys, partKey(sessionID, part.PartNo))
	}
	destKey := "blobs/" + uuid.NewString()

	hash, size, err := u.storage.Concat(ctx, destKey, srcKeys)
	if err != nil {
		return model.Node{}, fmt.Errorf("concat parts: %w", err)
	}
	blob, recorded, err := u.store.UpsertBlob(ctx, hash, size, u.backend, destKey)
	if err != nil {
		_ = u.storage.Delete(ctx, destKey) // 登记失败则删掉刚拼好的对象
		return model.Node{}, fmt.Errorf("upsert blob: %w", err)
	}

	if !recorded {
		if err := u.storage.Delete(ctx, destKey); err != nil &&
			!errors.Is(err, storage.ErrNotFound) {
			return model.Node{}, fmt.Errorf("delete duplicate blob: %w", err)
		}
	}
	node, err := u.store.CreateNode(
		ctx,
		ownerID,
		session.ParentID,
		session.Name,
		false,
		size,
		&blob.ID,
	)
	if err != nil {
		if errors.Is(err, repository.ErrUniqueViolation) {
			if recorded {
				deleted, cleanupErr := u.store.DeleteBlobIfUnreferenced(ctx, blob.ID)
				if cleanupErr == nil && deleted {
					_ = u.storage.Delete(ctx, destKey)
				}
			}

			return model.Node{}, ErrNameConflict
		}

		return model.Node{}, fmt.Errorf("create node: %w", err)
	}
	ok, err := u.store.TransitionUploadSession(
		ctx,
		ownerID,
		sessionID,
		"pending",
		"completed",
	)
	if err != nil {
		return model.Node{}, fmt.Errorf("complete upload session: %w", err)
	}
	if !ok {
		return model.Node{}, ErrUploadNotPending
	}
	// 节点已建、会话已 completed，删分片失败只留下垃圾，不影响本次收尾结果。
	for _, part := range parts {
		key := partKey(sessionID, part.PartNo)
		if err := u.storage.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			u.logger.Warn("delete completed part", "session_id", sessionID, "part_no", part.PartNo, "error", err)
		}
	}
	return node, nil
}

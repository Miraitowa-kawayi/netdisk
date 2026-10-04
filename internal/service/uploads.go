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

// Uploads 是分片上传（断点续传）的业务层。
//
// 会话就是一个状态机：pending（可传分片）→ completed（收尾成功、建了节点）
// 或 aborted（用户放弃）。分片按 part_no 存进 upload_parts，主键 (session_id, part_no)
// 让"重复传同一片"成为幂等操作 —— 这正是断点续传能工作的原因。
type Uploads struct {
	store   *repository.Store
	storage storage.Storage
	backend string // 写进 blobs.backend，和 Files 一致
	logger  *slog.Logger
}

func NewUploads(store *repository.Store, st storage.Storage, backend string, logger *slog.Logger) *Uploads {
	return &Uploads{store: store, storage: st, backend: backend, logger: logger}
}

// partKey 是分片对象在存储里的位置。和内容对象（blobs/…）分开前缀，一眼分得清。
func partKey(sessionID uuid.UUID, partNo int) string {
	return fmt.Sprintf("uploads/%s/%d", sessionID, partNo)
}

// checkParentDir 确认父目录存在、属于自己、且真的是个目录。nil = 根目录，永远存在。
// Files 与 Uploads 共用。
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
	ContentHash *string // 客户端预申报的整份 SHA-256（可选，P7 暂不据此秒传）
}

// Create 开一个分片会话。part_count 由 total_size/chunk_size 向上取整算出（至少 1）。
// 分片大小由**服务端**定（客户端给 chunk_size），这样每一片该多大是确定的，收尾才不会错位。
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

// Status 查会话进度。客户端（进程重启 / 上传中断后）拿 session id 回来问
// "我传了哪些、还缺哪些"，只补缺的那些 —— 这就是"断点"续上传。
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

// expectedPartSize 算第 partNo 片应有的字节数：除最后一片外都等于 chunk_size，
// 最后一片是总大小减掉前面所有片。于是"所有片大小之和 == total_size"是可证的。
func expectedPartSize(sess model.UploadSession, partNo int) int64 {
	if partNo == sess.PartCount-1 {
		return sess.TotalSize - int64(sess.PartCount-1)*sess.ChunkSize
	}
	return sess.ChunkSize
}

// PutPart 收一个分片：把 body **流式**写进存储，再登记行（重复传同一片就覆盖）。
//
// 大小严格校验：第 n 片必须正好是它该有的字节数，多一个少一个都拒。否则收尾拼出来的内容
// 会静默错位 —— 比"传失败了"难查得多。校验不过时把刚写下的对象删掉，不留垃圾。
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
	// 多读一个字节用来判"超长"：这样就算客户端灌一个超大 body，临时文件最多长到 want+1，
	// 不会被拖垮；真正的"正好 want 字节"由下面的 Stat 校验。
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

// Abort 放弃一个会话：标记 aborted 并清掉已收到的分片对象。已经不在 pending 的会话
// 再放弃也返回 nil（幂等）—— 用户点"取消"不该因为顺序而报错。
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

// Complete 收尾一个分片会话：确认分片齐了 → **按 part_no 顺序**拼成一份内容 →
// 登记 blob + 建节点 → 标记会话 completed → 清掉临时分片对象。返回建好的文件节点。
//
// 必须满足（internal/service/uploads_test.go 会逐条验）：
//  1. 会话不存在 / 不是自己的 → ErrNotFound；不是 pending（已完成或已放弃）→ ErrUploadNotPending；
//  2. 还缺分片 → ErrUploadIncomplete，且**什么都没变**（不建节点、不删分片、会话仍 pending）；
//  3. 分片齐全 → 按 part_no 升序拼接；建出的节点 size == total_size，
//     且内容的 SHA-256 == 把各分片按 part_no 顺序接起来的 SHA-256；
//  4. 收尾成功后：会话 status == completed、该会话的临时分片对象都被删掉；
//  5. 目标目录下已有同名 → ErrNameConflict（和普通上传同样的语义）；
//  6. 内容若已在库里（别人传过同一份）→ 复用那份 blob、删掉刚拼出来的副本（和 Files.Upload 一样）。
//
// 提示：拼接到新对象用 u.storage.Concat(ctx, dstKey, srcKeys)（srcKeys 按 part_no 升序），
// dstKey 用 "blobs/" + uuid.NewString()；登记与建节点照 Files.Upload 的三步走
// （UpsertBlob → 没新增就删掉自己刚写的副本 → CreateNode），最后用
// u.store.TransitionUploadSession 把会话推到 completed，并逐个删掉分片对象。
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
		_ = u.storage.Delete(ctx, destKey) // 记不上账就别把刚拼好的对象留在盘上
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
	// 清理分片是收尾的收尾：此刻节点已建、会话已 completed，删不掉只是留垃圾，
	// 不该把一次已经成功的收尾翻成 500。
	for _, part := range parts {
		key := partKey(sessionID, part.PartNo)
		if err := u.storage.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			u.logger.Warn("delete completed part", "session_id", sessionID, "part_no", part.PartNo, "error", err)
		}
	}
	return node, nil
}

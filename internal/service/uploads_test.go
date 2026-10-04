package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Miraitowa-kawayi/netdisk/internal/model"
	"github.com/Miraitowa-kawayi/netdisk/internal/repository"
	"github.com/Miraitowa-kawayi/netdisk/internal/storage"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const fallbackTestDSN = "postgres://netdisk:netdisk@localhost:5433/netdisk?sslmode=disable"

type uploadFixture struct {
	svc     *Uploads
	store   *repository.Store
	cleanDB *pgxpool.Pool // 只用来清理测试数据（删用户/删 blobs）与断言计数
	ctx     context.Context
	dir     string // 临时存储根
	owner   uuid.UUID
}

func newUploadFixture(t *testing.T) *uploadFixture {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = fallbackTestDSN
	}
	ctx := context.Background()

	st, err := repository.New(ctx, dsn)
	if err != nil {
		t.Skipf("跳过：建连接池失败（%v）", err)
	}
	if err := st.Ping(ctx); err != nil {
		st.Close()
		t.Skipf("跳过：数据库不可达（%v）—— 先跑 `make up`", err)
	}
	t.Cleanup(st.Close)

	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("跳过：清理用连接池建不起来（%v）", err)
	}
	t.Cleanup(db.Close)

	dir := t.TempDir()
	loc, err := storage.NewLocal(dir)
	if err != nil {
		t.Fatalf("建本地存储: %v", err)
	}
	svc := NewUploads(st, loc, "local", slog.New(slog.NewTextHandler(io.Discard, nil)))

	f := &uploadFixture{svc: svc, store: st, cleanDB: db, ctx: ctx, dir: dir}
	f.owner = f.newUser(t)
	return f
}

// newUser 建一个测试用户。users → upload_sessions / nodes 都是 ON DELETE CASCADE，
// 删掉用户就把这个用例造出来的会话、分片、节点全带走。
func (f *uploadFixture) newUser(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := f.store.CreateUser(f.ctx, "test-"+uuid.NewString(), "not-a-real-bcrypt-hash", "")
	if err != nil {
		t.Fatalf("建测试用户: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.cleanDB.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID)
	})
	return u.ID
}

// trackBlobHash 登记"用例结束后删掉这个 hash 的 blob 行"。blob 不属于任何用户，
// 不会随用户级联删除，所以要自己收。
func (f *uploadFixture) trackBlobHash(t *testing.T, hash string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = f.cleanDB.Exec(context.Background(), `DELETE FROM blobs WHERE content_hash = $1`, hash)
	})
}

func (f *uploadFixture) createSession(t *testing.T, owner uuid.UUID, name string, totalSize, chunkSize int64) model.UploadSession {
	t.Helper()
	sess, err := f.svc.Create(f.ctx, CreateInput{OwnerID: owner, Name: name, TotalSize: totalSize, ChunkSize: chunkSize})
	if err != nil {
		t.Fatalf("开分片会话: %v", err)
	}
	return sess
}

func (f *uploadFixture) putPart(t *testing.T, owner, sessionID uuid.UUID, partNo int, data []byte) {
	t.Helper()
	if _, err := f.svc.PutPart(f.ctx, owner, sessionID, partNo, bytes.NewReader(data)); err != nil {
		t.Fatalf("传第 %d 片: %v", partNo, err)
	}
}

func (f *uploadFixture) sessionStatus(t *testing.T, owner, sessionID uuid.UUID) string {
	t.Helper()
	sess, err := f.store.GetUploadSession(f.ctx, owner, sessionID)
	if err != nil {
		t.Fatalf("取会话: %v", err)
	}
	return sess.Status
}

func (f *uploadFixture) countNodes(t *testing.T, owner uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.cleanDB.QueryRow(f.ctx,
		`SELECT count(*) FROM nodes WHERE owner_id = $1 AND deleted_at IS NULL`, owner).Scan(&n); err != nil {
		t.Fatalf("数节点: %v", err)
	}
	return n
}

func (f *uploadFixture) countBlobsByHash(t *testing.T, hash string) int {
	t.Helper()
	var n int
	if err := f.cleanDB.QueryRow(f.ctx,
		`SELECT count(*) FROM blobs WHERE content_hash = $1`, hash).Scan(&n); err != nil {
		t.Fatalf("数 blob: %v", err)
	}
	return n
}

// storageFiles 数存储目录下的对象（不含目录）。用来验"分片被清掉 / 拼接副本被删 / 只留最终对象"。
func (f *uploadFixture) storageFiles(t *testing.T) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(f.dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历存储目录: %v", err)
	}
	return count
}

func (f *uploadFixture) readObject(t *testing.T, key string) []byte {
	t.Helper()
	rc, _, err := f.svc.storage.Open(f.ctx, key)
	if err != nil {
		t.Fatalf("读对象 %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读对象内容 %s: %v", key, err)
	}
	return b
}

// buildParts 造 totalSize 字节的可重复内容，按 chunkSize 切成片（空文件是"1 片、0 字节"）。
func buildParts(totalSize, chunkSize int64) [][]byte {
	data := make([]byte, totalSize)
	for i := range data {
		data[i] = byte(i*7%251 + 1)
	}
	if totalSize == 0 {
		return [][]byte{{}}
	}
	parts := make([][]byte, 0, (totalSize+chunkSize-1)/chunkSize)
	for off := int64(0); off < totalSize; off += chunkSize {
		end := off + chunkSize
		if end > totalSize {
			end = totalSize
		}
		parts = append(parts, data[off:end])
	}
	return parts
}

func hashParts(parts [][]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func joinParts(parts [][]byte) []byte {
	return bytes.Join(parts, nil)
}

// 缺片 → ErrUploadIncomplete，而且什么都没变（不建节点、不删分片、会话仍 pending）。
func TestCompleteRejectsIncompleteSession(t *testing.T) {
	f := newUploadFixture(t)
	parts := buildParts(3000, 1000) // 3 片
	sess := f.createSession(t, f.owner, "incomplete.bin", 3000, 1000)
	f.putPart(t, f.owner, sess.ID, 0, parts[0])
	f.putPart(t, f.owner, sess.ID, 2, parts[2]) // 故意缺第 1 片

	if _, err := f.svc.Complete(f.ctx, f.owner, sess.ID); !errors.Is(err, ErrUploadIncomplete) {
		t.Fatalf("缺片应收尾失败 ErrUploadIncomplete，实得 %v", err)
	}
	if got := f.sessionStatus(t, f.owner, sess.ID); got != "pending" {
		t.Errorf("会话状态应保持 pending，实得 %s", got)
	}
	if n := f.countNodes(t, f.owner); n != 0 {
		t.Errorf("缺片时不该建节点，实得 %d 个", n)
	}
	if files := f.storageFiles(t); files != 2 {
		t.Errorf("缺片时不该删分片，应有 2 个分片对象，实得 %d 个", files)
	}
}

// 分片齐全（且乱序到达）→ 按 part_no 顺序拼接，内容与按序拼接逐字节一致。
func TestCompleteAssemblesPartsInOrder(t *testing.T) {
	f := newUploadFixture(t)
	const total, chunk = int64(3000), int64(1000)
	parts := buildParts(total, chunk)
	sess := f.createSession(t, f.owner, "photo.bin", total, chunk)

	// 故意乱序上传：2 → 0 → 1。收尾必须按 part_no 拼，不能按"到达顺序"。
	f.putPart(t, f.owner, sess.ID, 2, parts[2])
	f.putPart(t, f.owner, sess.ID, 0, parts[0])
	f.putPart(t, f.owner, sess.ID, 1, parts[1])

	wantHash := hashParts(parts)
	f.trackBlobHash(t, wantHash)

	node, err := f.svc.Complete(f.ctx, f.owner, sess.ID)
	if err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	if node.Size != total {
		t.Errorf("node.size = %d, want %d", node.Size, total)
	}
	if node.BlobID == nil {
		t.Fatalf("节点没指向任何内容")
	}

	blob, err := f.store.GetBlobByID(f.ctx, *node.BlobID)
	if err != nil {
		t.Fatalf("取 blob: %v", err)
	}
	if blob.ContentHash != wantHash {
		t.Errorf("内容 hash = %s, want %s", blob.ContentHash, wantHash)
	}
	if blob.Size != total {
		t.Errorf("blob.size = %d, want %d", blob.Size, total)
	}
	if got := f.readObject(t, blob.StorageKey); !bytes.Equal(got, joinParts(parts)) {
		t.Errorf("拼出来的内容与按 part_no 顺序拼接的结果不一致")
	}

	if after := f.sessionStatus(t, f.owner, sess.ID); after != "completed" {
		t.Errorf("会话状态应为 completed，实得 %s", after)
	}
	if files := f.storageFiles(t); files != 1 {
		t.Errorf("收尾后应只剩最终对象（分片已清），实得 %d 个对象", files)
	}
}

// 别人的会话 → ErrNotFound。
func TestCompleteOtherOwnerIsNotFound(t *testing.T) {
	f := newUploadFixture(t)
	other := f.newUser(t)
	parts := buildParts(1000, 1000)
	sess := f.createSession(t, other, "theirs.bin", 1000, 1000)
	f.putPart(t, other, sess.ID, 0, parts[0])

	if _, err := f.svc.Complete(f.ctx, f.owner, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("收尾别人的会话应 ErrNotFound，实得 %v", err)
	}
}

// 重复收尾 → 第二次 ErrUploadNotPending，且不会多建一个节点。
func TestCompleteIsNotRepeatable(t *testing.T) {
	f := newUploadFixture(t)
	parts := buildParts(1000, 1000)
	sess := f.createSession(t, f.owner, "once.bin", 1000, 1000)
	f.putPart(t, f.owner, sess.ID, 0, parts[0])
	f.trackBlobHash(t, hashParts(parts))

	if _, err := f.svc.Complete(f.ctx, f.owner, sess.ID); err != nil {
		t.Fatalf("第一次收尾失败: %v", err)
	}
	if _, err := f.svc.Complete(f.ctx, f.owner, sess.ID); !errors.Is(err, ErrUploadNotPending) {
		t.Fatalf("重复收尾应 ErrUploadNotPending，实得 %v", err)
	}
	if n := f.countNodes(t, f.owner); n != 1 {
		t.Errorf("只应有一个节点，实得 %d 个", n)
	}
}

// 目标目录下已有同名 → ErrNameConflict；会话不该被标记 completed，也不该留下拼接副本。
func TestCompleteNameConflict(t *testing.T) {
	f := newUploadFixture(t)
	if _, err := f.store.CreateNode(f.ctx, f.owner, nil, "dup.bin", false, 1, nil); err != nil {
		t.Fatalf("预置同名节点: %v", err)
	}
	parts := buildParts(1000, 1000)
	sess := f.createSession(t, f.owner, "dup.bin", 1000, 1000)
	f.putPart(t, f.owner, sess.ID, 0, parts[0])
	wantHash := hashParts(parts)
	f.trackBlobHash(t, wantHash)

	if _, err := f.svc.Complete(f.ctx, f.owner, sess.ID); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("同名应收尾失败 ErrNameConflict，实得 %v", err)
	}
	if got := f.sessionStatus(t, f.owner, sess.ID); got != "pending" {
		t.Errorf("收尾失败后会话应仍是 pending，实得 %s", got)
	}
	if n := f.countBlobsByHash(t, wantHash); n != 0 {
		t.Errorf("同名冲突时该把已登记的 blob 行收回，实得 %d 行", n)
	}
	if files := f.storageFiles(t); files != 1 {
		t.Errorf("同名冲突时刚拼出来的副本应被清掉，盘上应只剩那 1 个分片，实得 %d 个", files)
	}
}

// 内容已在库里（别人传过同一份）→ 复用那份 blob、删掉刚拼出来的副本。
func TestCompleteReusesExistingContent(t *testing.T) {
	f := newUploadFixture(t)
	parts := buildParts(2000, 1000)
	wantHash := hashParts(parts)

	// 预置"这份内容早就在库里了"。
	existingKey := "blobs/existing-" + uuid.NewString()
	if _, recorded, err := f.store.UpsertBlob(f.ctx, wantHash, 2000, "local", existingKey); err != nil || !recorded {
		t.Fatalf("预置 blob: recorded=%v err=%v", recorded, err)
	}
	f.trackBlobHash(t, wantHash)

	sess := f.createSession(t, f.owner, "already.bin", 2000, 1000)
	f.putPart(t, f.owner, sess.ID, 0, parts[0])
	f.putPart(t, f.owner, sess.ID, 1, parts[1])

	node, err := f.svc.Complete(f.ctx, f.owner, sess.ID)
	if err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	if node.BlobID == nil {
		t.Fatalf("节点没指向内容")
	}
	blob, err := f.store.GetBlobByID(f.ctx, *node.BlobID)
	if err != nil {
		t.Fatalf("取 blob: %v", err)
	}
	if blob.StorageKey != existingKey {
		t.Errorf("应复用已存在的内容（key=%s），实得 key=%s", existingKey, blob.StorageKey)
	}
	if n := f.countBlobsByHash(t, wantHash); n != 1 {
		t.Errorf("该 hash 只应有一行 blob，实得 %d 行", n)
	}
	if files := f.storageFiles(t); files != 0 {
		t.Errorf("拼接副本应被删、分片应被清，盘上应为 0 个对象，实得 %d 个", files)
	}
}

// 空文件（1 片、0 字节）也能收尾。
func TestCompleteEmptyFile(t *testing.T) {
	f := newUploadFixture(t)
	parts := buildParts(0, 1024)
	if len(parts) != 1 || len(parts[0]) != 0 {
		t.Fatalf("空文件应切出 1 个 0 字节分片，实得 %d 片", len(parts))
	}
	sess := f.createSession(t, f.owner, "empty.bin", 0, 1024)
	if sess.PartCount != 1 {
		t.Fatalf("part_count = %d, want 1", sess.PartCount)
	}
	f.putPart(t, f.owner, sess.ID, 0, parts[0])
	f.trackBlobHash(t, hashParts(parts))

	node, err := f.svc.Complete(f.ctx, f.owner, sess.ID)
	if err != nil {
		t.Fatalf("收尾空文件失败: %v", err)
	}
	if node.Size != 0 {
		t.Errorf("node.size = %d, want 0", node.Size)
	}
}

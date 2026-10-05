package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/Miraitowa-kawayi/netdisk/internal/model"
	"github.com/google/uuid"
)

// 内容回收的用例打真数据库（先跑 `make up`），库不可达时跳过。

// newTestBlob 造一份 blobs 行；blob 无 owner，需在用例结束时自行清理。
// 清理先于用户执行（t.Cleanup 后进先出），此时节点还在，靠 ON DELETE SET NULL 删行。
func newTestBlob(t *testing.T, st *Store, ctx context.Context) model.Blob {
	t.Helper()
	sum := sha256.Sum256([]byte(uuid.NewString()))
	blob, recorded, err := st.UpsertBlob(ctx, hex.EncodeToString(sum[:]), 1234, "local", "blobs/"+uuid.NewString())
	if err != nil || !recorded {
		t.Fatalf("建测试 blob: recorded=%v err=%v", recorded, err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM blobs WHERE id = $1`, blob.ID)
	})
	return blob
}

// newTestFile 在 parent 下建一个指向 blob 的文件节点。
func newTestFile(t *testing.T, st *Store, ctx context.Context, owner uuid.UUID, parent *uuid.UUID, name string, blobID uuid.UUID) uuid.UUID {
	t.Helper()
	n, err := st.CreateNode(ctx, owner, parent, name, false, 1234, &blobID)
	if err != nil {
		t.Fatalf("建文件 %s: %v", name, err)
	}
	return n.ID
}

// 子树里唯一引用某内容的文件被删时，该 blob 行消失且 key 出现在返回值里。
func TestReclaimOrphanBlobsDeletesUnreferencedContent(t *testing.T) {
	st, ctx := newTestStore(t)
	owner := newTestUser(t, st, ctx)

	dir, err := st.CreateNode(ctx, owner, nil, "A", true, 0, nil)
	if err != nil {
		t.Fatalf("建目录: %v", err)
	}
	blob := newTestBlob(t, st, ctx)
	newTestFile(t, st, ctx, owner, &dir.ID, "a.bin", blob.ID)

	if err := st.SoftDeleteSubtree(ctx, owner, dir.ID); err != nil {
		t.Fatalf("SoftDeleteSubtree: %v", err)
	}

	keys, err := st.ReclaimOrphanBlobs(ctx, owner, dir.ID)
	if err != nil {
		t.Fatalf("不该返回错误，实得 %v", err)
	}
	if len(keys) != 1 || keys[0] != blob.StorageKey {
		t.Errorf("回收到的 key = %v, want [%s]", keys, blob.StorageKey)
	}
	if _, err := st.GetBlobByHash(ctx, blob.ContentHash); !errors.Is(err, ErrNotFound) {
		t.Errorf("blob 行应已删除，GetBlobByHash = %v", err)
	}
}

// 同一 blob 在子树里被多个文件引用时 key 只出现一次。
func TestReclaimOrphanBlobsDedupesWithinSubtree(t *testing.T) {
	st, ctx := newTestStore(t)
	owner := newTestUser(t, st, ctx)

	dir, err := st.CreateNode(ctx, owner, nil, "A", true, 0, nil)
	if err != nil {
		t.Fatalf("建目录: %v", err)
	}
	blob := newTestBlob(t, st, ctx)
	newTestFile(t, st, ctx, owner, &dir.ID, "a.bin", blob.ID)
	newTestFile(t, st, ctx, owner, &dir.ID, "b.bin", blob.ID)

	if err := st.SoftDeleteSubtree(ctx, owner, dir.ID); err != nil {
		t.Fatalf("SoftDeleteSubtree: %v", err)
	}

	keys, err := st.ReclaimOrphanBlobs(ctx, owner, dir.ID)
	if err != nil {
		t.Fatalf("不该返回错误，实得 %v", err)
	}
	if len(keys) != 1 {
		t.Errorf("同一份内容只该回收一次，实得 %d 个 key: %v", len(keys), keys)
	}
}

// 子树外仍有存活节点引用同一 blob 时一行都不删。
func TestReclaimOrphanBlobsKeepsLiveReference(t *testing.T) {
	st, ctx := newTestStore(t)
	owner := newTestUser(t, st, ctx)

	dir, err := st.CreateNode(ctx, owner, nil, "A", true, 0, nil)
	if err != nil {
		t.Fatalf("建目录: %v", err)
	}
	blob := newTestBlob(t, st, ctx)
	newTestFile(t, st, ctx, owner, &dir.ID, "a.bin", blob.ID)
	newTestFile(t, st, ctx, owner, nil, "keep.bin", blob.ID) // 根目录上的第二个引用，保留

	if err := st.SoftDeleteSubtree(ctx, owner, dir.ID); err != nil {
		t.Fatalf("SoftDeleteSubtree: %v", err)
	}

	keys, err := st.ReclaimOrphanBlobs(ctx, owner, dir.ID)
	if err != nil {
		t.Fatalf("不该返回错误，实得 %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("还有存活引用，不该回收，实得 %v", keys)
	}
	if _, err := st.GetBlobByHash(ctx, blob.ContentHash); err != nil {
		t.Errorf("blob 行应还在，GetBlobByHash = %v", err)
	}
}

// 空目录返回空切片 + nil。
func TestReclaimOrphanBlobsEmptyDir(t *testing.T) {
	st, ctx := newTestStore(t)
	owner := newTestUser(t, st, ctx)

	dir, err := st.CreateNode(ctx, owner, nil, "empty", true, 0, nil)
	if err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := st.SoftDeleteSubtree(ctx, owner, dir.ID); err != nil {
		t.Fatalf("SoftDeleteSubtree: %v", err)
	}

	keys, err := st.ReclaimOrphanBlobs(ctx, owner, dir.ID)
	if err != nil {
		t.Fatalf("删空目录不该是错误，实得 %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("空目录没有内容可回收，实得 %v", keys)
	}
}

// 别的 owner 的存活节点引用同一 blob 时同样不删。
func TestReclaimOrphanBlobsKeepsOtherOwnerReference(t *testing.T) {
	st, ctx := newTestStore(t)
	owner := newTestUser(t, st, ctx)
	other := newTestUser(t, st, ctx)

	dir, err := st.CreateNode(ctx, owner, nil, "A", true, 0, nil)
	if err != nil {
		t.Fatalf("建目录: %v", err)
	}
	blob := newTestBlob(t, st, ctx)
	newTestFile(t, st, ctx, owner, &dir.ID, "a.bin", blob.ID)
	newTestFile(t, st, ctx, other, nil, "shared.bin", blob.ID)

	if err := st.SoftDeleteSubtree(ctx, owner, dir.ID); err != nil {
		t.Fatalf("SoftDeleteSubtree: %v", err)
	}

	keys, err := st.ReclaimOrphanBlobs(ctx, owner, dir.ID)
	if err != nil {
		t.Fatalf("不该返回错误，实得 %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("别的用户还在用，不该回收，实得 %v", keys)
	}
	if _, err := st.GetBlobByHash(ctx, blob.ContentHash); err != nil {
		t.Errorf("blob 行应还在，GetBlobByHash = %v", err)
	}
}

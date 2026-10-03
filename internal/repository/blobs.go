package repository

import (
	"context"

	"github.com/Miraitowa-kawayi/netdisk/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const blobColumns = `id, content_hash, size, backend, storage_key, created_at`

func scanBlob(row pgx.Row) (model.Blob, error) {
	var b model.Blob
	err := row.Scan(&b.ID, &b.ContentHash, &b.Size, &b.Backend, &b.StorageKey, &b.CreatedAt)
	return b, translate(err)
}

// GetBlobByHash 按内容哈希查一份内容。查不到返回 ErrNotFound。
func (s *Store) GetBlobByHash(ctx context.Context, hash string) (model.Blob, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+blobColumns+` FROM blobs WHERE content_hash = $1`, hash)
	return scanBlob(row)
}

// GetBlobByID 按 id 查一份内容（下载时由 node.blob_id 反查）。
func (s *Store) GetBlobByID(ctx context.Context, id uuid.UUID) (model.Blob, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+blobColumns+` FROM blobs WHERE id = $1`, id)
	return scanBlob(row)
}

// UpsertBlob 登记一份内容：同 hash 的行已存在就复用，否则插入。
//
// recorded 表示"这次真插入了新行"。false 表示内容早就在库里（别人传过或自己传过），
// 调用方应该把刚刚写出的那份副本删掉 —— 磁盘上只留最早那一份。
//
// 写成 INSERT ... ON CONFLICT DO NOTHING + 回查的一条 CTE，是为了让并发上传同一
// 内容时也只有一个赢家：输的那个拿到赢家的行，而不是报唯一键错误。
func (s *Store) UpsertBlob(ctx context.Context, hash string, size int64, backend, storageKey string) (blob model.Blob, recorded bool, err error) {
	const q = `
WITH ins AS (
    INSERT INTO blobs (content_hash, size, backend, storage_key)
    VALUES ($1, $2, $3, $4)
    ON CONFLICT (content_hash) DO NOTHING
    RETURNING ` + blobColumns + `, true AS recorded
)
SELECT ` + blobColumns + `, recorded FROM ins
UNION ALL
SELECT ` + blobColumns + `, false AS recorded FROM blobs WHERE content_hash = $1
LIMIT 1`

	row := s.pool.QueryRow(ctx, q, hash, size, backend, storageKey)
	err = row.Scan(&blob.ID, &blob.ContentHash, &blob.Size, &blob.Backend, &blob.StorageKey, &blob.CreatedAt, &recorded)
	return blob, recorded, translate(err)
}

// DeleteBlobIfUnreferenced 在没有未删节点引用这份内容时删掉 blob 行，返回是否真删了。
//
// 这里用 NOT EXISTS 现算，而不是维护 blobs.ref_count 计数列：计数列漏加一次就会
// 永久漂移，最后表现为"文件删光了磁盘内容却永远删不掉"（见 docs/schema.md 第 3 节）。
func (s *Store) DeleteBlobIfUnreferenced(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
DELETE FROM blobs
 WHERE id = $1
   AND NOT EXISTS (SELECT 1 FROM nodes WHERE blob_id = $1 AND deleted_at IS NULL)`, id)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() > 0, nil
}

// ===========================================================================
//  以下是 D3 的引用计数核心 —— 留给你写。契约在注释里，验证命令见文件末尾。
// ===========================================================================

// ReclaimOrphanBlobs 回收一次删除所波及的内容，返回可以从磁盘上删掉的 storage_key。
//
// rootID 是**刚刚被 SoftDeleteSubtree 弱删掉**的子树根。两步：
//
//  1. 沿 parent_id 把 rootID 这棵子树展开，收集里面的文件节点引用过的 blob（按 id 去重；
//     目录的 blob_id 是 NULL，要排除）。⚠️ 这些行此刻的 deleted_at **已经非空**，
//     所以这一步不能像别处那样带 `deleted_at IS NULL`，否则一行都查不到。
//  2. 只删其中"现在已经没有任何**存活**节点引用"的 blob 行。
//     存活判定必须**全局**看（不带 owner_id），不能只看这棵子树：
//     别人目录里、别的用户那边还有人在用的内容绝不能删。
//
// 返回被删掉的那些 blob 的 storage_key —— 调用方据此删磁盘对象。
// 节点那一侧的 blob_id 指针由外键 `ON DELETE SET NULL` 自动清掉（见 migrations/0002）。
// 没有可回收的内容时返回空切片 + nil —— 删一个空文件夹不是错误。
//
// 必须满足（internal/repository/blobs_test.go 会逐条验）：
//  1. 子树里唯一引用某内容的文件被删 → 该 blob 行消失、它的 key 出现在返回值里；
//  2. 同一个 blob 在子树里被多个文件引用 → key 只出现一次；
//  3. 子树外还有存活节点引用同一 blob → 一行都不删，返回空；
//  4. 空目录 → 空切片 + nil；
//  5. 别的 owner 的存活节点引用同一 blob → 同样不删。
//
// 提示：`WITH RECURSIVE ... , victims AS ( SELECT DISTINCT ... ) DELETE FROM blobs ...
// RETURNING storage_key` 可以在一条 SQL 里做完；起点用 `id = $1 AND owner_id = $2`，
// 往下扩展用 `JOIN ... ON n.parent_id = s.id`。
func (s *Store) ReclaimOrphanBlobs(ctx context.Context, ownerID, rootID uuid.UUID) ([]string, error) {
	return nil, ErrNotImplemented
}

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

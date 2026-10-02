package repository

import (
	"context"

	"github.com/Miraitowa-kawayi/netdisk/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const nodeColumns = `id, owner_id, parent_id, name, is_dir, size, blob_id, created_at, updated_at`

func scanNode(row pgx.Row) (model.Node, error) {
	var n model.Node
	err := row.Scan(&n.ID, &n.OwnerID, &n.ParentID, &n.Name, &n.IsDir, &n.Size, &n.BlobID, &n.CreatedAt, &n.UpdatedAt)
	return n, translate(err)
}

// CreateNode 建一个文件或文件夹节点。
// 同层同名会撞 nodes_sibling_name_uniq → ErrUniqueViolation。
func (s *Store) CreateNode(ctx context.Context, ownerID uuid.UUID, parentID *uuid.UUID, name string, isDir bool, size int64, blobID *uuid.UUID) (model.Node, error) {
	row := s.pool.QueryRow(ctx, `
INSERT INTO nodes (owner_id, parent_id, name, is_dir, size, blob_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING `+nodeColumns,
		ownerID, parentID, name, isDir, size, blobID)
	return scanNode(row)
}

// GetNode 取节点。owner_id 一起进 WHERE —— 别人的节点在这里表现为 ErrNotFound，
// 而不是 403，这样连"这个 id 存在不存在"都不会泄露。
func (s *Store) GetNode(ctx context.Context, ownerID, id uuid.UUID) (model.Node, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL`,
		id, ownerID)
	return scanNode(row)
}

// ListChildren 列某目录的直接子项。parentID 为 nil 表示根目录。
// IS NOT DISTINCT FROM 是为了让 "parent_id IS NULL" 和 "parent_id = $2" 用同一条语句表达。
func (s *Store) ListChildren(ctx context.Context, ownerID uuid.UUID, parentID *uuid.UUID) ([]model.Node, error) {
	rows, err := s.pool.Query(ctx, `
SELECT `+nodeColumns+`
  FROM nodes
 WHERE owner_id = $1
   AND deleted_at IS NULL
   AND parent_id IS NOT DISTINCT FROM $2::uuid
 ORDER BY is_dir DESC, name`, ownerID, parentID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	nodes := make([]model.Node, 0, 16)
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, translate(rows.Err())
}

// RenameNode 改名。文件与文件夹同表，所以同一条 UPDATE 同时管两者。
// 重名 → ErrUniqueViolation；节点不存在 → ErrNotFound。
func (s *Store) RenameNode(ctx context.Context, ownerID, id uuid.UUID, name string) (model.Node, error) {
	row := s.pool.QueryRow(ctx, `
UPDATE nodes SET name = $3, updated_at = now()
 WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL
RETURNING `+nodeColumns,
		id, ownerID, name)
	return scanNode(row)
}

// SoftDeleteNode 弱删：只打 deleted_at。唯一索引带 WHERE deleted_at IS NULL，
// 所以删掉之后可以立刻再建一个同名的。
//
// 磁盘内容（blob）的回收故意不在这里做 —— 那需要引用计数，是 D3 的工作。
func (s *Store) SoftDeleteNode(ctx context.Context, ownerID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE nodes SET deleted_at = now(), updated_at = now()
 WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL`, id, ownerID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

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

// CreateNode 建一个文件或文件夹节点；同层同名返回 ErrUniqueViolation。
func (s *Store) CreateNode(ctx context.Context, ownerID uuid.UUID, parentID *uuid.UUID, name string, isDir bool, size int64, blobID *uuid.UUID) (model.Node, error) {
	row := s.pool.QueryRow(ctx, `
INSERT INTO nodes (owner_id, parent_id, name, is_dir, size, blob_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING `+nodeColumns,
		ownerID, parentID, name, isDir, size, blobID)
	return scanNode(row)
}

// GetNode 取节点；owner_id 一并进 WHERE，别人的节点返回 ErrNotFound。
func (s *Store) GetNode(ctx context.Context, ownerID, id uuid.UUID) (model.Node, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+nodeColumns+` FROM nodes WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL`,
		id, ownerID)
	return scanNode(row)
}

// ListChildren 列某目录的直接子项，parentID 为 nil 表示根目录。
// IS NOT DISTINCT FROM 使 parent_id IS NULL 与 parent_id = $2 共用一条语句。
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

// UpdateNode 改名和/或移动：name 为 nil 表示不改名，setParent 为 false 表示不动父目录。
// setParent 用于区分"移到根目录（parentID = nil）"与"不修改父目录"。
// 同层重名返回 ErrUniqueViolation，节点不存在返回 ErrNotFound。
func (s *Store) UpdateNode(ctx context.Context, ownerID, id uuid.UUID, name *string, parentID *uuid.UUID, setParent bool) (model.Node, error) {
	row := s.pool.QueryRow(ctx, `
UPDATE nodes SET name       = COALESCE($3, name),
                 parent_id  = CASE WHEN $4 THEN $5::uuid ELSE parent_id END,
                 updated_at = now()
 WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL
RETURNING `+nodeColumns,
		id, ownerID, name, setParent, parentID)
	return scanNode(row)
}

// SoftDeleteSubtree 软删一个节点及其整棵子树（递归 CTE 一条语句完成）。
// 唯一索引带 WHERE deleted_at IS NULL，删除后可立即重建同名。
// 节点不存在或已被删返回 ErrNotFound。
func (s *Store) SoftDeleteSubtree(ctx context.Context, ownerID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
WITH RECURSIVE subtree AS (
    SELECT id FROM nodes WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL
    UNION ALL
    SELECT n.id FROM nodes n JOIN subtree ON n.parent_id = subtree.id
     WHERE n.owner_id = $2 AND n.deleted_at IS NULL
)
UPDATE nodes SET deleted_at = now(), updated_at = now()
 WHERE id IN (SELECT id FROM subtree)`, id, ownerID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IsSelfOrDescendant 判断 candidate 是 ancestor 自身还是其后代。
// 任一节点不存在时返回 (false, nil)。
func (s *Store) IsSelfOrDescendant(ctx context.Context, ownerID, candidateID, ancestorID uuid.UUID) (bool, error) {
	const query = `
	WITH RECURSIVE ancestors AS (
		SELECT id, parent_id
		FROM nodes
		WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL
		UNION ALL
		SELECT n.id, n.parent_id
		FROM nodes n
		JOIN ancestors a ON n.id = a.parent_id
		WHERE n.owner_id = $2 AND n.deleted_at IS NULL
	) CYCLE id SET is_cycle USING path
	SELECT EXISTS (
		SELECT 1 FROM ancestors WHERE id = $3
	)`

	var isDescendant bool
	err := s.pool.QueryRow(ctx, query, candidateID, ownerID, ancestorID).Scan(&isDescendant)
	if err != nil {
		return false, translate(err)
	}

	return isDescendant, nil
}

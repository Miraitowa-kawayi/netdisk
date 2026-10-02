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

// UpdateNode 改名和/或移动。文件与文件夹同表，所以同一条 UPDATE 同时管两者。
//
// name 为 nil 表示不改名；setParent 为 false 表示不动父目录 ——
// 这两个"不设置"的开关是必需的：只靠 nil 区分不出"移到根目录（parent_id = NULL）"
// 和"别碰 parent_id"。
//
// 同层重名 → ErrUniqueViolation；节点不存在 → ErrNotFound。
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

// SoftDeleteSubtree 弱删一个节点**以及它的整棵子树**。
//
// 递归 CTE 从目标节点往下沿 parent_id 展开，一条语句标记完整棵树 ——
// 在 Go 里逐层递归会变成 N 次数据库往返。
//
// 唯一索引带 WHERE deleted_at IS NULL，所以删掉之后可以立刻再建同名的。
// 磁盘内容（blob）的回收故意不在这里做 —— 那需要引用计数，是 D3 的工作。
//
// 节点不存在（或已被删）→ ErrNotFound。
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

// IsSelfOrDescendant 判断 candidate 是不是 ancestor 自己、或者 ancestor 的子孙。
//
// D2 的用法：把节点 A 移进目录 D 之前，先问
//
//	IsSelfOrDescendant(ctx, ownerID, candidate=D, ancestor=A)
//
// 返回 true 就说明 D 落在 A 的子树里（含 D == A）—— 这一移会让树成环，
// 必须拒绝（service 会翻成 ErrCycle）。
//
// 必须满足（internal/repository/nodes_test.go 会逐条验）：
//  1. 用**一条递归 CTE** 解决：沿 parent_id 从 candidate 往上爬，看路上有没有 ancestor。
//     不许在 Go 里一层层 GetNode 循环 —— 那样一次移动就是 N 次往返。
//  2. candidate == ancestor 时返回 true（"自己"也算）。
//  3. 方向不能反：ancestor 在 candidate 上面 → true；反过来 → false。
//  4. 一路只看未删除的节点（deleted_at IS NULL），并且 owner_id 要带在每一层里
//     —— 别人的节点不算我的子孙。
//  5. candidate 或 ancestor 不存在 → 返回 (false, nil)。"不是我的子孙"是一个答案，
//     不是故障，别返回错误。
//
// 提示：`WITH RECURSIVE x AS ( 起点 UNION ALL 沿边往上扩展 )` 就是干这个的；
// 往"上"爬的写法是 JOIN ON n.id = x.parent_id。PG 14+ 还有 CYCLE 子句，
// 可以防"数据里本来就有环"导致的无限递归。
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

// 验证命令（需要数据库在跑，先 `make up`）：
//
//	go test ./internal/repository/ -run IsSelfOrDescendant -v
//
// 数据库不可达时这些用例会 **SKIP** 而不是失败 —— 所以看到 SKIP 不算过。

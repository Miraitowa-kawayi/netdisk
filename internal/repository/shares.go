package repository

import (
	"context"
	"time"

	"github.com/Miraitowa-kawayi/netdisk/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const shareColumns = `id, token, node_id, created_by, expires_at, visit_count, created_at`

func scanShare(row pgx.Row) (model.Share, error) {
	var s model.Share
	err := row.Scan(&s.ID, &s.Token, &s.NodeID, &s.CreatedBy, &s.ExpiresAt, &s.VisitCount, &s.CreatedAt)
	return s, translate(err)
}

// CreateShare 建一条分享。token 由 service 生成（随机串），expiresAt 为 nil 表示永不过期。
func (s *Store) CreateShare(ctx context.Context, nodeID, createdBy uuid.UUID, token string, expiresAt *time.Time) (model.Share, error) {
	row := s.pool.QueryRow(ctx, `
INSERT INTO shares (token, node_id, created_by, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING `+shareColumns, token, nodeID, createdBy, expiresAt)
	return scanShare(row)
}

// GetShareByToken 按公开 token 取分享 —— 匿名访问的唯一入口，所以这里**没有** owner_id 条件：
// 拿着 token 的人就是被授权的人。
func (s *Store) GetShareByToken(ctx context.Context, token string) (model.Share, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+shareColumns+` FROM shares WHERE token = $1`, token)
	return scanShare(row)
}

// ListSharesByOwner 列某人建过的分享，新的在前。
func (s *Store) ListSharesByOwner(ctx context.Context, ownerID uuid.UUID) ([]model.Share, error) {
	rows, err := s.pool.Query(ctx, `
SELECT `+shareColumns+`
  FROM shares
 WHERE created_by = $1
 ORDER BY created_at DESC`, ownerID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	shares := make([]model.Share, 0, 8)
	for rows.Next() {
		sh, err := scanShare(rows)
		if err != nil {
			return nil, err
		}
		shares = append(shares, sh)
	}
	return shares, translate(rows.Err())
}

// DeleteShare 撤销一条分享。owner_id 一起进 WHERE —— 撤销别人的分享表现为 ErrNotFound，
// 和 GetNode 一样"不区分不存在与不属于你"。
func (s *Store) DeleteShare(ctx context.Context, ownerID, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM shares WHERE id = $1 AND created_by = $2`, id, ownerID)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IncrementShareVisit 给访问计数 +1。只做展示用 —— 调用方失败只 WARN，不该影响访问本身。
func (s *Store) IncrementShareVisit(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE shares SET visit_count = visit_count + 1 WHERE id = $1`, id)
	return translate(err)
}

// NodeIsLive 判断节点存在、且**从它到根的整条祖先链上没有任何一环被弱删**。
//
// 为什么单独需要它：`shares.node_id` 是 ON DELETE CASCADE，但 nodes 是弱删的 ——
// 那个 CASCADE 在"删掉一个目录"这条主路径上永远不会触发。于是"文件自己没被删、但它的
// 父目录被删了"这种情况只能靠这条查询在访问时挡下来（docs/schema.md 第 6 节的约定）。
//
// 两个 EXISTS 合起来表达"链路存在" 且 "链路上没有一环 deleted_at 非空"。
// ⚠️ 起点和递归都**不能**带 `deleted_at IS NULL` —— 正是要把被删的那一环查出来，
// 带了就变成"永远为真"。
func (s *Store) NodeIsLive(ctx context.Context, ownerID, id uuid.UUID) (bool, error) {
	const query = `
WITH RECURSIVE up AS (
    SELECT id, parent_id, deleted_at
      FROM nodes
     WHERE id = $1 AND owner_id = $2
    UNION ALL
    SELECT n.id, n.parent_id, n.deleted_at
      FROM nodes n
      JOIN up ON n.id = up.parent_id
     WHERE n.owner_id = $2
) CYCLE id SET is_cycle USING path
SELECT EXISTS (SELECT 1 FROM up)
   AND NOT EXISTS (SELECT 1 FROM up WHERE deleted_at IS NOT NULL)`

	var live bool
	if err := s.pool.QueryRow(ctx, query, id, ownerID).Scan(&live); err != nil {
		return false, translate(err)
	}
	return live, nil
}

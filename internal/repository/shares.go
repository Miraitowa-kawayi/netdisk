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

// CreateShare 建一条分享；token 由 service 生成，expiresAt 为 nil 表示永不过期。
func (s *Store) CreateShare(ctx context.Context, nodeID, createdBy uuid.UUID, token string, expiresAt *time.Time) (model.Share, error) {
	row := s.pool.QueryRow(ctx, `
INSERT INTO shares (token, node_id, created_by, expires_at)
VALUES ($1, $2, $3, $4)
RETURNING `+shareColumns, token, nodeID, createdBy, expiresAt)
	return scanShare(row)
}

// GetShareByToken 按公开 token 取分享，不带 owner_id 条件（token 即授权凭据）。
func (s *Store) GetShareByToken(ctx context.Context, token string) (model.Share, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+shareColumns+` FROM shares WHERE token = $1`, token)
	return scanShare(row)
}

// ListSharesByOwner 列某人创建的分享，新的在前。
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

// DeleteShare 撤销一条分享；owner_id 一并进 WHERE，撤销别人的分享返回 ErrNotFound。
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

// IncrementShareVisit 给访问计数 +1；调用方失败只记 WARN，不影响访问。
func (s *Store) IncrementShareVisit(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE shares SET visit_count = visit_count + 1 WHERE id = $1`, id)
	return translate(err)
}

// NodeIsLive 判断节点存在，且从它到根的整条祖先链上没有任何一环被软删。
// 起点和递归都不能带 deleted_at IS NULL，因为正需要查出被删的那一环；带上会使结果恒为真。
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

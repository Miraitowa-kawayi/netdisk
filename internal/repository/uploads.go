package repository

import (
	"context"

	"github.com/Miraitowa-kawayi/netdisk/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const uploadSessionColumns = `id, owner_id, parent_id, name, total_size, chunk_size, part_count, status, content_hash, created_at, expires_at`

func scanUploadSession(row pgx.Row) (model.UploadSession, error) {
	var s model.UploadSession
	err := row.Scan(&s.ID, &s.OwnerID, &s.ParentID, &s.Name, &s.TotalSize, &s.ChunkSize,
		&s.PartCount, &s.Status, &s.ContentHash, &s.CreatedAt, &s.ExpiresAt)
	return s, translate(err)
}

// CreateUploadSession 开一次分片上传。partCount 由 service 按 totalSize/chunkSize 算好传进来
// （把算术放在业务层，SQL 只管存）。expires_at 走 DDL 默认值（1 天后）。
func (s *Store) CreateUploadSession(ctx context.Context, ownerID uuid.UUID, parentID *uuid.UUID, name string, totalSize, chunkSize int64, partCount int, contentHash *string) (model.UploadSession, error) {
	row := s.pool.QueryRow(ctx, `
INSERT INTO upload_sessions (owner_id, parent_id, name, total_size, chunk_size, part_count, content_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING `+uploadSessionColumns,
		ownerID, parentID, name, totalSize, chunkSize, partCount, contentHash)
	return scanUploadSession(row)
}

// GetUploadSession 取会话。owner_id 也进 WHERE —— 别人的会话表现为 ErrNotFound
// （和 GetNode 一样的"不区分不存在与不属于你"）。
func (s *Store) GetUploadSession(ctx context.Context, ownerID, id uuid.UUID) (model.UploadSession, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+uploadSessionColumns+` FROM upload_sessions WHERE id = $1 AND owner_id = $2`,
		id, ownerID)
	return scanUploadSession(row)
}

// ListPendingUploadSessions 列某用户还没收尾的会话（客户端丢了 session id 时找回）。
func (s *Store) ListPendingUploadSessions(ctx context.Context, ownerID uuid.UUID) ([]model.UploadSession, error) {
	rows, err := s.pool.Query(ctx, `
SELECT `+uploadSessionColumns+`
  FROM upload_sessions
 WHERE owner_id = $1 AND status = 'pending'
 ORDER BY created_at DESC`, ownerID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	sessions := make([]model.UploadSession, 0, 4)
	for rows.Next() {
		sess, err := scanUploadSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, translate(rows.Err())
}

// PutUploadPart 登记一个已收到的分片。主键 (session_id, part_no) 让重复上传同一分片
// 变成幂等 upsert —— 断点续传里客户端重传同一片是正常操作，后传的覆盖先传的。
func (s *Store) PutUploadPart(ctx context.Context, sessionID uuid.UUID, partNo int, size int64, checksum string) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO upload_parts (session_id, part_no, size, checksum)
VALUES ($1, $2, $3, $4)
ON CONFLICT (session_id, part_no)
DO UPDATE SET size = EXCLUDED.size, checksum = EXCLUDED.checksum, created_at = now()`,
		sessionID, partNo, size, checksum)
	return translate(err)
}

// ListUploadParts 列会话已收到的分片，**按 part_no 升序** —— 这是收尾时合并顺序的事实来源
// （不要依赖行在表里的物理顺序，也不要用"收到的先后顺序"）。
func (s *Store) ListUploadParts(ctx context.Context, sessionID uuid.UUID) ([]model.UploadPart, error) {
	rows, err := s.pool.Query(ctx, `
SELECT session_id, part_no, size, checksum, created_at
  FROM upload_parts
 WHERE session_id = $1
 ORDER BY part_no`, sessionID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	parts := make([]model.UploadPart, 0, 8)
	for rows.Next() {
		var p model.UploadPart
		if err := rows.Scan(&p.SessionID, &p.PartNo, &p.Size, &p.Checksum, &p.CreatedAt); err != nil {
			return nil, translate(err)
		}
		parts = append(parts, p)
	}
	return parts, translate(rows.Err())
}

// TransitionUploadSession 原子地把会话状态从 from 迁到 to，返回是否真的迁移了。
// `UPDATE ... WHERE status = $from` 就是一次 compare-and-set：并发下只有一个调用能拿到 true，
// 所以"收尾只能成一次"不靠读-改-写。
func (s *Store) TransitionUploadSession(ctx context.Context, ownerID, id uuid.UUID, from, to string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
UPDATE upload_sessions SET status = $4
 WHERE id = $1 AND owner_id = $2 AND status = $3`, id, ownerID, from, to)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() > 0, nil
}

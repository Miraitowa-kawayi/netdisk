// Package repository 是数据访问层：所有 SQL 都在这里，上层不直接碰数据库。
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 收敛过的驱动层错误，让上层不用认识 pgx。
var (
	// ErrNotFound 表示按主键/唯一键没查到记录。
	ErrNotFound = errors.New("repository: not found")
	// ErrUniqueViolation 表示撞上了唯一约束（用户名、同层同名……）。
	ErrUniqueViolation = errors.New("repository: unique violation")
	// ErrNotImplemented 是尚未实现的骨架方法的占位错误。难点方法由人补上之前，
	// 调用它会明确失败，而不是悄悄返回零值（false / 空列表）。
	ErrNotImplemented = errors.New("repository: not implemented")
)

// translate 把 pgx 的错误翻成上面两个哨兵，其余原样返回。
func translate(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrUniqueViolation
	}
	return err
}

// Store 持有连接池。后续各实体的查询方法（Users/Nodes/Blobs/Shares/Uploads）
// 都挂在它上面，按主题拆成同包的多个文件。
type Store struct {
	pool *pgxpool.Pool
}

// New 建连接池。注意 pgxpool 是**惰性连接**：这里不校验数据库是否可达，
// 启动时的可用性检查放在 main 里的 Ping（或 /readyz）里做。
func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create pgx pool: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close 关闭连接池，等待在用连接归还。
func (s *Store) Close() { s.pool.Close() }

// Ping 验证数据库可达，供启动检查与 /readyz 使用。
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

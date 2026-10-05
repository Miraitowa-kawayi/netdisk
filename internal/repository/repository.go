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

// 收敛后的驱动层错误，上层无需认识 pgx。
var (
	// ErrNotFound 表示按主键或唯一键未查到记录。
	ErrNotFound = errors.New("repository: not found")
	// ErrUniqueViolation 表示违反唯一约束（用户名、同层同名等）。
	ErrUniqueViolation = errors.New("repository: unique violation")
	// ErrNotImplemented 是骨架方法的占位错误，调用时明确失败而非返回零值。
	ErrNotImplemented = errors.New("repository: not implemented")
)

// translate 把 pgx 错误映射为上面的哨兵错误，其余原样返回。
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

// Store 持有连接池，各实体的查询方法按主题拆在同包的多个文件里。
type Store struct {
	pool *pgxpool.Pool
}

// New 建连接池。pgxpool 惰性连接，此处不校验可达性（启动检查在 Ping 或 /readyz）。
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

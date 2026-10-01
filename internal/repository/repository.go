// Package repository 是数据访问层：所有 SQL 都在这里，上层不直接碰数据库。
package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

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

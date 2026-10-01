// Package model 是数据库表在 Go 侧的映射，字段与 migrations/0001_init.sql 一一对应。
//
// 这里刻意不引入 ORM 的模型标签：数据访问用 pgx 手写 SQL，
// 表结构的事实来源是 migrations 里的 DDL，不是结构体标签。
package model

import (
	"time"

	"github.com/google/uuid"
)

// User 对应 users 表。密码哈希永不序列化到 JSON。
type User struct {
	ID           uuid.UUID `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Nickname     string    `json:"nickname"`
	Bio          string    `json:"bio"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Blob 是磁盘上真实存在的一份内容，多个 Node 可以指向同一个 Blob（秒传）。
type Blob struct {
	ID          uuid.UUID `json:"id"`
	ContentHash string    `json:"content_hash"`
	Size        int64     `json:"size"`
	Backend     string    `json:"backend"`
	StorageKey  string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

// Node 是文件或文件夹。二者同表：is_dir 区分，parent_id 自引用成树。
type Node struct {
	ID        uuid.UUID  `json:"id"`
	OwnerID   uuid.UUID  `json:"owner_id"`
	ParentID  *uuid.UUID `json:"parent_id"` // nil = 根目录
	Name      string     `json:"name"`
	IsDir     bool       `json:"is_dir"`
	Size      int64      `json:"size"`
	BlobID    *uuid.UUID `json:"-"` // 目录为 nil
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Share 是一条分享链接。token 是公开访问凭据。
type Share struct {
	ID         uuid.UUID  `json:"id"`
	Token      string     `json:"token"`
	NodeID     uuid.UUID  `json:"node_id"`
	CreatedBy  uuid.UUID  `json:"created_by"`
	ExpiresAt  *time.Time `json:"expires_at"`
	VisitCount int64      `json:"visit_count"`
	CreatedAt  time.Time  `json:"created_at"`
}

// UploadSession 是一次分片上传的意图，P7 断点续传的状态主体。
type UploadSession struct {
	ID          uuid.UUID  `json:"id"`
	OwnerID     uuid.UUID  `json:"owner_id"`
	ParentID    *uuid.UUID `json:"parent_id"`
	Name        string     `json:"name"`
	TotalSize   int64      `json:"total_size"`
	ChunkSize   int64      `json:"chunk_size"`
	PartCount   int        `json:"part_count"`
	Status      string     `json:"status"` // pending | completed | aborted
	ContentHash *string    `json:"content_hash"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
}

// UploadPart 是会话里已收到的一个分片。主键 (SessionID, PartNo) 保证重传幂等。
type UploadPart struct {
	SessionID uuid.UUID `json:"session_id"`
	PartNo    int       `json:"part_no"`
	Size      int64     `json:"size"`
	Checksum  string    `json:"checksum"`
	CreatedAt time.Time `json:"created_at"`
}

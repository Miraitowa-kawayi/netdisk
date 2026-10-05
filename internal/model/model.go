// Package model 是数据库表的 Go 映射，字段与 migrations/0001_init.sql 对应。
package model

import (
	"time"

	"github.com/google/uuid"
)

// User 对应 users 表；PasswordHash 不序列化。
type User struct {
	ID           uuid.UUID `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Nickname     string    `json:"nickname"`
	Bio          string    `json:"bio"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Blob 是一份存储内容，多个 Node 可指向同一个 Blob。
type Blob struct {
	ID          uuid.UUID `json:"id"`
	ContentHash string    `json:"content_hash"`
	Size        int64     `json:"size"`
	Backend     string    `json:"backend"`
	StorageKey  string    `json:"-"`
	CreatedAt   time.Time `json:"created_at"`
}

// Node 是文件或文件夹，由 is_dir 区分，parent_id 自引用成树。
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

// Share 是一条分享链接，token 为公开访问凭据。
type Share struct {
	ID         uuid.UUID  `json:"id"`
	Token      string     `json:"token"`
	NodeID     uuid.UUID  `json:"node_id"`
	CreatedBy  uuid.UUID  `json:"created_by"`
	ExpiresAt  *time.Time `json:"expires_at"`
	VisitCount int64      `json:"visit_count"`
	CreatedAt  time.Time  `json:"created_at"`
}

// UploadSession 是一次分片上传会话。
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

// UploadPart 是已收到的一个分片，主键 (SessionID, PartNo) 保证重传幂等。
type UploadPart struct {
	SessionID uuid.UUID `json:"session_id"`
	PartNo    int       `json:"part_no"`
	Size      int64     `json:"size"`
	Checksum  string    `json:"checksum"`
	CreatedAt time.Time `json:"created_at"`
}

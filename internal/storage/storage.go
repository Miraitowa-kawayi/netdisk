// Package storage 抽象内容对象的存取。
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound 表示 key 对应的对象不存在。
var ErrNotFound = errors.New("storage: object not found")

// ObjectInfo 是对象的元信息。
type ObjectInfo struct {
	Key  string
	Size int64
	Hash string // SHA-256 十六进制
}

// Storage 是内容存取接口；实现必须保证读写都不把整个对象载入内存。
type Storage interface {
	// Put 流式写入对象，返回内容的 SHA-256（十六进制小写）；失败时不留半个对象。
	Put(ctx context.Context, key string, r io.Reader, size int64) (hash string, err error)

	// Open 打开对象供读取；返回 ReadSeekCloser 以便 http.ServeContent 支持 Range。
	Open(ctx context.Context, key string) (io.ReadSeekCloser, ObjectInfo, error)

	// Concat 按 srcKeys 的顺序拼成新对象 dstKey，返回 (SHA-256, 总字节数)，全程流式。
	// 任一源对象不存在时返回 ErrNotFound，失败不留半成品。
	Concat(ctx context.Context, dstKey string, srcKeys []string) (hash string, size int64, err error)

	// Delete 删除对象。对象不存在时返回 ErrNotFound。
	Delete(ctx context.Context, key string) error

	// Stat 返回对象元信息。
	Stat(ctx context.Context, key string) (ObjectInfo, error)
}

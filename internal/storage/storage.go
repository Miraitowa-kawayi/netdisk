// Package storage 把"内容存在哪里"抽象成一个接口。
//
// 存在的意义：P1 只会有 local 实现，P6 要加对象存储（s3）。
// 上层（service/handler）只依赖这个接口，P6 因此不必回头改 P1 的代码。
// 实现从 D1 开始写。
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound 表示 key 对应的对象不存在。上层用它区分"404"和"真的出错了"。
var ErrNotFound = errors.New("storage: object not found")

// ObjectInfo 是对象的元信息。
type ObjectInfo struct {
	Key  string
	Size int64
	Hash string // SHA-256 十六进制
}

// Storage 是内容存取接口。
//
// key 由调用方决定（本项目用 blobs.storage_key）。
// 实现必须满足：读/写都不把整个对象载入内存。
type Storage interface {
	// Put 流式写入对象，返回内容的 SHA-256（十六进制小写）。
	// 写到一半失败时不得留下半个对象。
	Put(ctx context.Context, key string, r io.Reader, size int64) (hash string, err error)

	// Open 打开对象供读取。返回 ReadSeekCloser 是为了让 http.ServeContent
	// 能直接拿到 Range 支持（它需要 Seek 来定位区间）。
	Open(ctx context.Context, key string) (io.ReadSeekCloser, ObjectInfo, error)

	// Concat 把 srcKeys 里的对象按**给定顺序**拼成一个新对象 dstKey，
	// 返回新对象的 (SHA-256, 总字节数)。全程流式：一次只读一个源对象，
	// 内存不随总大小涨。任一源对象不存在 → ErrNotFound，且最终路径上不留半成品。
	//
	// 顺序由调用方负责 —— 这一层只管"你给的顺序，我照着接"。
	Concat(ctx context.Context, dstKey string, srcKeys []string) (hash string, size int64, err error)

	// Delete 删除对象。对象不存在时返回 ErrNotFound。
	Delete(ctx context.Context, key string) error

	// Stat 返回对象元信息。
	Stat(ctx context.Context, key string) (ObjectInfo, error)
}

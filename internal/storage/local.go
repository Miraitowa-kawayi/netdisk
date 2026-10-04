package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Local 把对象存在本地文件系统上，磁盘布局就是 <dir>/<key>。
//
// 由 service 层决定 key（本项目是 "blobs/<uuid>"）；这一层不关心业务，只管字节。
type Local struct {
	dir string
}

func NewLocal(dir string) (*Local, error) {
	if dir == "" {
		return nil, errors.New("storage: local dir is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: create dir %s: %w", dir, err)
	}
	return &Local{dir: dir}, nil
}

// keyPath 把 key 映射成磁盘路径。
// 注意 path.Clean("/"+key) 的那一撇：它把 "../../etc/passwd" 这种 key 归一化成
// 根目录内部的路径，所以 key 没法逃出 l.dir。
func (l *Local) keyPath(key string) (string, error) {
	if key == "" {
		return "", errors.New("storage: empty key")
	}
	p := filepath.Join(l.dir, filepath.FromSlash(path.Clean("/"+key)))

	rel, err := filepath.Rel(l.dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("storage: key %q escapes the root", key)
	}
	return p, nil
}

// ===========================================================================
//  以下两个是 D1 的流式核心 —— 留给你写。契约在注释里，验证命令见文件末尾。
// ===========================================================================

// Put 流式写入对象，返回内容的 SHA-256（十六进制小写）。
//
// 必须满足（local_test.go 会逐条验）：
//  1. 边读 r 边写盘 —— 不许 io.ReadAll，内存不能随对象大小涨；
//  2. r 只读一遍：hash 用 io.TeeReader / io.MultiWriter 在写的路上顺便算；
//  3. 写成功之前，最终路径上不能出现文件 —— 先写临时文件，成了再 rename 过去；
//  4. 中途出错要把临时文件清掉，最终路径上不留半成品；
//  5. 返回 hex.EncodeToString(hasher.Sum(nil))。
//
// 提示：os.CreateTemp(目标目录, ".put-*") 建临时文件能保证同一文件系统，
// rename 才是原子的。
func (l *Local) Put(ctx context.Context, key string, r io.Reader, size int64) (string, error) {
	// 获取安全的绝对路径
	destPath, err := l.keyPath(key)
	if err != nil {
		return "", err
	}

	// 确保目标目录存在
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	// 创建临时文件
	tempFile, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		return "", fmt.Errorf("storage: create temp file: %w", err)
	}
	defer tempFile.Close()

	// defer 清理临时文件，如果出错或中途取消
	var success bool
	defer func() {
		tempFile.Close()
		if !success {
			os.Remove(tempFile.Name())
		}
	}()

	// 边读 r 边写盘，同时计算 SHA-256（使用io.MultiWriter分流）
	hasher := sha256.New()
	mw := io.MultiWriter(tempFile, hasher)

	if _, err := io.Copy(mw, r); err != nil {
		return "", fmt.Errorf("storage: copy to temp file: %w", err)
	}

	// 刷盘并关闭临时文件，避免remane时数据未写入磁盘
	if err := tempFile.Sync(); err != nil {
		return "", fmt.Errorf("storage: sync temp file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return "", fmt.Errorf("storage: close temp file: %w", err)
	}

	// 原子替换
	if err := os.Rename(tempFile.Name(), destPath); err != nil {
		return "", fmt.Errorf("storage: rename temp file: %w", err)
	}
	success = true

	// 返回 SHA-256 哈希值
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// Open 打开对象供读取。返回的 reader 必须能 Seek ——
// http.ServeContent 靠它支持 Range/206（D4 断点续传的下载半边）。
// 对象不存在时返回 ErrNotFound。
func (l *Local) Open(ctx context.Context, key string) (io.ReadSeekCloser, ObjectInfo, error) {
	destPath, err := l.keyPath(key)
	if err != nil {
		return nil, ObjectInfo{}, err
	}

	f, err := os.Open(destPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ObjectInfo{}, ErrNotFound
		}
		return nil, ObjectInfo{}, err
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, ObjectInfo{}, err
	}

	info := ObjectInfo{
		Key:  key,
		Size: fi.Size(),
	}
	return f, info, nil
}

func (l *Local) Delete(ctx context.Context, key string) error {
	p, err := l.keyPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (l *Local) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	p, err := l.keyPath(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, err
	}
	return ObjectInfo{Key: key, Size: fi.Size()}, nil
}

// Concat 把 srcKeys 里的对象按给定顺序拼成一个新对象 dstKey，边拼边算 SHA-256。
//
// 和 Put 是同一个套路（临时文件 → Sync → Close → Rename），区别是"源"从**一个 Reader**
// 变成**一串已存在的对象**：逐个 Open、读出、写进同一个 MultiWriter。一次只开一个源文件，
// 所以内存和文件描述符都不随分片数涨。任一源对象不存在 → ErrNotFound，
// 中途失败时最终路径上不留半成品（临时文件建在目标目录，defer 清掉）。
//
// 这是 D4 分片上传收尾要用的原语：把 uploads/<session>/<n> 按 n 的顺序接起来。
func (l *Local) Concat(ctx context.Context, dstKey string, srcKeys []string) (string, int64, error) {
	destPath, err := l.keyPath(dstKey)
	if err != nil {
		return "", 0, err
	}
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}

	tempFile, err := os.CreateTemp(dir, ".concat-*")
	if err != nil {
		return "", 0, fmt.Errorf("storage: create temp file: %w", err)
	}

	var success bool
	defer func() {
		if !success {
			tempFile.Close()
			os.Remove(tempFile.Name())
		}
	}()

	hasher := sha256.New()
	mw := io.MultiWriter(tempFile, hasher)

	var total int64
	for _, key := range srcKeys {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		src, _, err := l.Open(ctx, key)
		if err != nil {
			return "", 0, err // 源不存在 → 直接是 ErrNotFound，不留半成品
		}
		n, err := io.Copy(mw, src)
		src.Close()
		if err != nil {
			return "", 0, fmt.Errorf("storage: concat %q: %w", key, err)
		}
		total += n
	}

	if err := tempFile.Sync(); err != nil {
		return "", 0, fmt.Errorf("storage: sync temp file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return "", 0, fmt.Errorf("storage: close temp file: %w", err)
	}
	if err := os.Rename(tempFile.Name(), destPath); err != nil {
		return "", 0, fmt.Errorf("storage: rename temp file: %w", err)
	}
	success = true

	return hex.EncodeToString(hasher.Sum(nil)), total, nil
}

// 编译期断言：Local 必须满足 Storage 接口（漏实现方法在这一行就会报错）。
var _ Storage = (*Local)(nil)

// 验证命令（在 netdisk/ 目录下跑）：
//
//	go test ./internal/storage/ -v
//
// 全绿之后，再跑一次真正的端到端：
//
//	go run ./cmd/server
//	curl -s -X POST localhost:8081/api/v1/auth/register -H 'Content-Type: application/json' \
//	-d '{"username":"alice","password":"password123"}'
//	TOKEN=$(curl -s -X POST localhost:8081/api/v1/auth/login -H 'Content-Type: application/json' \
//	    -d '{"username":"alice","password":"password123"}' | jq -r .token)
//	head -c 5000000 /dev/urandom > /tmp/big.bin
//  curl -s -X POST localhost:8081/api/v1/files -H "Authorization: Bearer $TOKEN" -F file=@/tmp/big.bin//

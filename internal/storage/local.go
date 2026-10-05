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

// Local 把对象存在本地文件系统上，布局为 <dir>/<key>。
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

// keyPath 把 key 映射成磁盘路径；path.Clean("/"+key) 使 key 无法逃出 l.dir。
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

// Put 流式写入对象，返回内容的 SHA-256（十六进制小写）。
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

// Open 打开对象供读取，返回的 reader 必须可 Seek（http.ServeContent 依赖它支持 Range）。
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

// Concat 按 srcKeys 的顺序把对象拼成新对象 dstKey，边拼边算 SHA-256，返回 (hash, 总字节数)。
// 一次只开一个源对象，内存不随分片数涨；任一源对象不存在时返回 ErrNotFound，失败不留半成品。
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
			return "", 0, err // 源不存在时直接返回 ErrNotFound，不留半成品
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

// 编译期断言 Local 实现 Storage。
var _ Storage = (*Local)(nil)

package service

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/Miraitowa-kawayi/netdisk/internal/model"
	"github.com/Miraitowa-kawayi/netdisk/internal/repository"
	"github.com/Miraitowa-kawayi/netdisk/internal/storage"
	"github.com/google/uuid"
)

const maxNameLen = 255

// Files 实现文件的上传、下载、改名、删除与列表。
type Files struct {
	store   *repository.Store
	storage storage.Storage
	backend string // 写进 blobs.backend，local | s3
	logger  *slog.Logger
}

func NewFiles(store *repository.Store, st storage.Storage, backend string, logger *slog.Logger) *Files {
	return &Files{store: store, storage: st, backend: backend, logger: logger}
}

// UploadInput 是一次上传。Body 尚未读完，Upload 会在读取过程中写入存储。
type UploadInput struct {
	OwnerID  uuid.UUID
	ParentID *uuid.UUID // nil = 根目录
	Name     string
	SizeHint int64 // 拿不到就传 -1；真实大小以实际读到的字节数为准
	Body     io.Reader
}

// Upload 把 Body 边读边写进存储，登记 blob，再建节点。
func (f *Files) Upload(ctx context.Context, in UploadInput) (model.Node, error) {
	name, err := cleanName(in.Name)
	if err != nil {
		return model.Node{}, err
	}
	if err := f.checkParent(ctx, in.OwnerID, in.ParentID); err != nil {
		return model.Node{}, err
	}

	// 先写自己专属的 key，避免并发上传互相覆盖。
	key := "blobs/" + uuid.NewString()

	counted := &countingReader{r: in.Body}
	hash, err := f.storage.Put(ctx, key, counted, in.SizeHint)
	if err != nil {
		return model.Node{}, fmt.Errorf("write content: %w", err)
	}

	blob, recorded, err := f.store.UpsertBlob(ctx, hash, counted.n, f.backend, key)
	if err != nil {
		_ = f.storage.Delete(ctx, key) // 登记失败则删掉刚写的对象
		return model.Node{}, err
	}
	if !recorded {
		// 内容已存在：删掉刚写的副本，复用库里的对象。
		if err := f.storage.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return model.Node{}, fmt.Errorf("drop duplicate object: %w", err)
		}
	}

	node, err := f.store.CreateNode(ctx, in.OwnerID, in.ParentID, name, false, blob.Size, &blob.ID)
	if err != nil {
		// 并发重名被唯一索引挡下：收回本次登记。
		if recorded {
			f.cleanupBlob(ctx, blob.ID, blob.StorageKey)
		}
		if errors.Is(err, repository.ErrUniqueViolation) {
			return model.Node{}, ErrNameConflict
		}
		return model.Node{}, err
	}
	return node, nil
}

// cleanupBlob 收回一次失败的登记：先删行，再删对象。
func (f *Files) cleanupBlob(ctx context.Context, blobID uuid.UUID, storageKey string) {
	deleted, err := f.store.DeleteBlobIfUnreferenced(ctx, blobID)
	if err != nil || !deleted {
		return // 仍有别的节点引用，不删对象
	}
	_ = f.storage.Delete(ctx, storageKey)
}

// InstantInput 是一次秒传请求：按客户端提供的 SHA-256 复用已存内容。
type InstantInput struct {
	OwnerID  uuid.UUID
	ParentID *uuid.UUID // nil = 根目录
	Name     string
	Hash     string // 客户端算好的 SHA-256（十六进制，大小写都收）
	Size     int64  // 只作参照；真实大小以库里 blob.size 为准
}

// InstantUpload 按内容 hash 命中已有 blob 就直接建节点，不接收字节。
// 节点大小以 blobs.size 为准，in.Size 仅用于一致性校验。
func (f *Files) InstantUpload(ctx context.Context, in InstantInput) (model.Node, error) {
	name, err := cleanName(in.Name)
	if err != nil {
		return model.Node{}, err
	}
	hash, err := cleanHash(in.Hash)
	if err != nil {
		return model.Node{}, err
	}
	if err := f.checkParent(ctx, in.OwnerID, in.ParentID); err != nil {
		return model.Node{}, err
	}

	blob, err := f.store.GetBlobByHash(ctx, hash)
	if errors.Is(err, repository.ErrNotFound) {
		return model.Node{}, ErrContentNotStored
	}
	if err != nil {
		return model.Node{}, err
	}
	if in.Size > 0 && in.Size != blob.Size {
		return model.Node{}, invalid("size 与已存内容的实际大小不符")
	}

	node, err := f.store.CreateNode(ctx, in.OwnerID, in.ParentID, name, false, blob.Size, &blob.ID)
	if errors.Is(err, repository.ErrUniqueViolation) {
		return model.Node{}, ErrNameConflict
	}
	if err != nil {
		return model.Node{}, err
	}
	return node, nil
}

// List 列目录的直接子项；目录不存在或不是文件夹时返回错误而不是空列表。
func (f *Files) List(ctx context.Context, ownerID uuid.UUID, parentID *uuid.UUID) ([]model.Node, error) {
	if err := f.checkParent(ctx, ownerID, parentID); err != nil {
		return nil, err
	}
	return f.store.ListChildren(ctx, ownerID, parentID)
}

// CreateDir 新建文件夹。
func (f *Files) CreateDir(ctx context.Context, ownerID uuid.UUID, parentID *uuid.UUID, name string) (model.Node, error) {
	name, err := cleanName(name)
	if err != nil {
		return model.Node{}, err
	}
	if err := f.checkParent(ctx, ownerID, parentID); err != nil {
		return model.Node{}, err
	}

	node, err := f.store.CreateNode(ctx, ownerID, parentID, name, true, 0, nil)
	if errors.Is(err, repository.ErrUniqueViolation) {
		return model.Node{}, ErrNameConflict
	}
	if err != nil {
		return model.Node{}, err
	}
	return node, nil
}

// UpdateInput 是一次修改请求，两个字段都可选。
type UpdateInput struct {
	Name      *string // 非 nil → 改名
	SetParent bool    // true → 移动（ParentID 为 nil 表示移回根目录）
	ParentID  *uuid.UUID
}

// Update 改名和/或移动一个节点。
func (f *Files) Update(ctx context.Context, ownerID, id uuid.UUID, in UpdateInput) (model.Node, error) {
	var name *string
	if in.Name != nil {
		cleaned, err := cleanName(*in.Name)
		if err != nil {
			return model.Node{}, err
		}
		name = &cleaned
	}

	if in.SetParent {
		if err := f.checkParent(ctx, ownerID, in.ParentID); err != nil {
			return model.Node{}, err
		}

		// 移到根目录（ParentID == nil）不可能成环，跳过环检测。
		if in.ParentID != nil {
			cycle, err := f.store.IsSelfOrDescendant(ctx, ownerID, *in.ParentID, id)
			if err != nil {
				return model.Node{}, err
			}
			if cycle {
				return model.Node{}, ErrCycle
			}
		}
	}

	node, err := f.store.UpdateNode(ctx, ownerID, id, name, in.ParentID, in.SetParent)
	switch {
	case errors.Is(err, repository.ErrUniqueViolation):
		return model.Node{}, ErrNameConflict
	case errors.Is(err, repository.ErrNotFound):
		return model.Node{}, ErrNotFound
	case err != nil:
		return model.Node{}, err
	}
	return node, nil
}

// Delete 软删节点，删文件夹时连同整棵子树一起。
// 删除后回收不再被任何节点引用的内容；回收失败不影响删除结果，只记日志。
func (f *Files) Delete(ctx context.Context, ownerID, id uuid.UUID) error {
	if err := f.store.SoftDeleteSubtree(ctx, ownerID, id); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}

	keys, err := f.store.ReclaimOrphanBlobs(ctx, ownerID, id)
	if err != nil {
		f.logger.Warn("reclaim orphan blobs after delete", "node_id", id, "error", err)
		return nil
	}
	for _, key := range keys {
		if err := f.storage.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			// 行已删、对象没删掉，只会在磁盘上留下垃圾，不影响读路径。
			f.logger.Warn("delete reclaimed object", "key", key, "error", err)
		}
	}
	return nil
}

// Download 是一次下载需要的全部东西。
type Download struct {
	Name    string
	Size    int64
	ModTime time.Time
	Content io.ReadSeekCloser // 必须可 Seek：http.ServeContent 依赖它支持 Range
}

// OpenDownload 打开一个文件供下载；所有权校验由 GetNode 完成。
func (f *Files) OpenDownload(ctx context.Context, ownerID, id uuid.UUID) (Download, error) {
	node, err := f.store.GetNode(ctx, ownerID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return Download{}, ErrNotFound
	}
	if err != nil {
		return Download{}, err
	}
	if node.BlobID == nil {
		return Download{}, ErrNotAFile
	}

	blob, err := f.store.GetBlobByID(ctx, *node.BlobID)
	if err != nil {
		return Download{}, err
	}

	rc, _, err := f.storage.Open(ctx, blob.StorageKey)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return Download{}, ErrNotFound
		}
		return Download{}, err
	}
	return Download{Name: node.Name, Size: blob.Size, ModTime: node.UpdatedAt, Content: rc}, nil
}

// ZipDownload 是一次文件夹打包下载。
type ZipDownload struct {
	Name    string        // 建议的文件名（含 .zip）
	Content io.ReadCloser // 边遍历边产生的 zip 字节流
}

// OpenZip 打开一个文件夹的打包下载。
//
// 校验（节点存在、属于调用者、是目录）在返回前完成；遍历打包经 io.Pipe 在后台进行，
// 开始输出字节后的错误只能通过流传递给读端。
func (f *Files) OpenZip(ctx context.Context, ownerID, id uuid.UUID) (ZipDownload, error) {
	root, err := f.store.GetNode(ctx, ownerID, id)
	if errors.Is(err, repository.ErrNotFound) {
		return ZipDownload{}, ErrNotFound
	}
	if err != nil {
		return ZipDownload{}, err
	}
	if !root.IsDir {
		return ZipDownload{}, ErrNotADirectory
	}

	pr, pw := io.Pipe()
	go func() {
		zw := zip.NewWriter(pw)
		// 出错时不再调用 zw.Close：否则读端会拿到一个空但合法的 zip。
		if err := f.writeZip(ctx, zw, root); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := zw.Close(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()

	return ZipDownload{Name: root.Name + ".zip", Content: pr}, nil
}

// writeZip 把 root 的整棵子树写进 zw，条目名以 root.Name + "/" 为前缀。
func (f *Files) writeZip(ctx context.Context, zw *zip.Writer, root model.Node) error {
	rootPrefix := strings.Trim(root.Name, "/") + "/"
	rootHeader := &zip.FileHeader{
		Name:     rootPrefix,
		Method:   zip.Store,
		Modified: root.UpdatedAt,
	}
	if _, err := zw.CreateHeader(rootHeader); err != nil {
		return fmt.Errorf("create root dir entry: %w", err)
	}

	var walk func(parentID *uuid.UUID, currentPrefix string) error
	walk = func(parentID *uuid.UUID, currentPrefix string) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		children, err := f.store.ListChildren(ctx, root.OwnerID, parentID)
		if err != nil {
			return err
		}

		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return err
			}

			childPrefix := currentPrefix + child.Name

			if child.IsDir {
				dirHeader := &zip.FileHeader{
					Name:     childPrefix + "/",
					Method:   zip.Store,
					Modified: child.UpdatedAt,
				}
				if _, err := zw.CreateHeader(dirHeader); err != nil {
					return fmt.Errorf("create dir entry: %w", err)
				}
				childID := child.ID
				if err := walk(&childID, childPrefix+"/"); err != nil {
					return err
				}
			} else {
				fileHeader := &zip.FileHeader{
					Name:     childPrefix,
					Method:   getCompressionMethod(child.Name),
					Modified: child.UpdatedAt,
				}

				writer, err := zw.CreateHeader(fileHeader)
				if err != nil {
					return fmt.Errorf("create file entry: %w", err)
				}

				if child.BlobID == nil {
					continue
				}

				blob, err := f.store.GetBlobByID(ctx, *child.BlobID)
				if err != nil {
					return fmt.Errorf("get blob: %w", err)
				}

				if err := func() error {
					reader, _, err := f.storage.Open(ctx, blob.StorageKey)
					if err != nil {
						return fmt.Errorf("open file content: %w", err)
					}
					defer reader.Close()

					_, err = io.Copy(writer, reader)
					return err
				}(); err != nil {
					return fmt.Errorf("write file content: %w", err)
				}
			}
		}
		return nil
	}

	rootID := root.ID
	return walk(&rootID, rootPrefix)
}

// getCompressionMethod 按扩展名挑压缩方式：已压缩的格式直接 Store，其余 Deflate。
func getCompressionMethod(name string) uint16 {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".jpg", ".png", ".gif", ".zip", ".mp4", ".mp3":
		return zip.Store
	default:
		return zip.Deflate
	}
}

// checkParent 确认父目录存在、属于自己、且真的是个目录。
func (f *Files) checkParent(ctx context.Context, ownerID uuid.UUID, parentID *uuid.UUID) error {
	return checkParentDir(ctx, f.store, ownerID, parentID)
}

// countingReader 统计实际读取的字节数，作为 blob.size 的来源。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// cleanName 只留最后一段路径，挡掉 ../ 、空名和超长名。
func cleanName(raw string) (string, error) {
	raw = strings.ReplaceAll(raw, "\\", "/") // 浏览器偶尔会送 Windows 全路径
	name := path.Base(strings.TrimSpace(raw))
	if name == "" || name == "." || name == "/" || name == ".." {
		return "", invalid("文件名不能为空")
	}
	if len(name) > maxNameLen {
		return "", invalid("文件名最长 %d 字节", maxNameLen)
	}
	return name, nil
}

// cleanHash 校验并规范化客户端上报的 SHA-256（统一为小写十六进制）。
func cleanHash(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(raw))
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != sha256.Size {
		return "", invalid("hash 必须是 64 位十六进制的 SHA-256")
	}
	return h, nil
}

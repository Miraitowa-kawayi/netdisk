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

// Files 是文件业务层：上传、下载、改名、删除、列表的规则都在这里。
type Files struct {
	store   *repository.Store
	storage storage.Storage
	backend string // 写进 blobs.backend，local | s3
	logger  *slog.Logger
}

func NewFiles(store *repository.Store, st storage.Storage, backend string, logger *slog.Logger) *Files {
	return &Files{store: store, storage: st, backend: backend, logger: logger}
}

// UploadInput 是一次上传。Body 是**还没被读完的请求体** ——
// Upload 会把它在读取过程中写进存储，这就是 D1"全程流式"的落点。
type UploadInput struct {
	OwnerID  uuid.UUID
	ParentID *uuid.UUID // nil = 根目录
	Name     string
	SizeHint int64 // 拿不到就传 -1；真实大小以实际读到的字节数为准
	Body     io.Reader
}

// Upload 把 Body 边读边写进存储，登记 blobs，再建 nodes。
//
// 三步：写内容（顺便算出 hash）→ 登记 blobs（同 hash 复用）→ 建节点。
// 注意这还不是"秒传"：客户端仍然把字节传上来了，只是服务端发现内容早就有、
// 于是丢掉刚写的副本（真正的秒传是 D3：客户端报 hash，一个字节都不传）。
func (f *Files) Upload(ctx context.Context, in UploadInput) (model.Node, error) {
	name, err := cleanName(in.Name)
	if err != nil {
		return model.Node{}, err
	}
	if err := f.checkParent(ctx, in.OwnerID, in.ParentID); err != nil {
		return model.Node{}, err
	}

	// 每次上传先写到自己专属的 key，两个并发上传不会互相踩。
	key := "blobs/" + uuid.NewString()

	counted := &countingReader{r: in.Body}
	hash, err := f.storage.Put(ctx, key, counted, in.SizeHint)
	if err != nil {
		return model.Node{}, fmt.Errorf("write content: %w", err)
	}

	blob, recorded, err := f.store.UpsertBlob(ctx, hash, counted.n, f.backend, key)
	if err != nil {
		_ = f.storage.Delete(ctx, key) // 记不上账就别把这个对象留在盘上
		return model.Node{}, err
	}
	if !recorded {
		// 这份内容库里已经有了：复用它的对象，删掉刚写的副本，磁盘上只留一份。
		if err := f.storage.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return model.Node{}, fmt.Errorf("drop duplicate object: %w", err)
		}
	}

	node, err := f.store.CreateNode(ctx, in.OwnerID, in.ParentID, name, false, blob.Size, &blob.ID)
	if err != nil {
		// 唯一索引终究可能挡下并发下的漏网重名：把这次刚建立的东西收回。
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

// cleanupBlob 收回一次失败的登记：先删行（只在没有别的节点引用时），再删对象。
func (f *Files) cleanupBlob(ctx context.Context, blobID uuid.UUID, storageKey string) {
	deleted, err := f.store.DeleteBlobIfUnreferenced(ctx, blobID)
	if err != nil || !deleted {
		return // 还有别的节点引用它，对象不能删
	}
	_ = f.storage.Delete(ctx, storageKey)
}

// InstantInput 是一次"秒传"请求：客户端声称本地文件的 SHA-256 是 Hash，
// 服务端如果已经有这份内容，就一个字节都不收，直接建节点。
type InstantInput struct {
	OwnerID  uuid.UUID
	ParentID *uuid.UUID // nil = 根目录
	Name     string
	Hash     string // 客户端算好的 SHA-256（十六进制，大小写都收）
	Size     int64  // 只作参照；真实大小以库里 blob.size 为准
}

// InstantUpload 秒传：按内容 hash 命中已有 blob 就直接建节点。
//
// 和 Upload 的区别：Upload 是"边收边写、写完了再看内容是不是早就有"，
// 秒传是"客户端先算好 hash 报上来"，所以命中的话网络上一个字节都不动 ——
// 第二个用户传同一个文件才会"瞬间完成"且磁盘不增长。
//
// size 不听客户端的：内容的事实来源是 blobs.size。客户端报的 size 只用来
// 早期发现"hash 对了但 size 差很多"这种明显不一致，不改写库的值。
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

// List 列目录的直接子项。目录不存在（或不是文件夹）时返回错误而不是空列表，
// 免得客户端把"路径写错"和"目录是空的"混起来。
func (f *Files) List(ctx context.Context, ownerID uuid.UUID, parentID *uuid.UUID) ([]model.Node, error) {
	if err := f.checkParent(ctx, ownerID, parentID); err != nil {
		return nil, err
	}
	return f.store.ListChildren(ctx, ownerID, parentID)
}

// CreateDir 新建文件夹。文件与文件夹同表，所以和 Upload 只差 is_dir/size/blob 三个字段。
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

// UpdateInput 是 PATCH /files/{id} 的一次修改，两个字段都可选。
type UpdateInput struct {
	Name      *string // 非 nil → 改名
	SetParent bool    // true → 移动（ParentID 为 nil 表示移回根目录）
	ParentID  *uuid.UUID
}

// Update 改名和/或移动一个节点。
//
// 顺序很重要：先确认目标目录真实存在且是目录，再做环检测，最后才写库。
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

		// —— D2 的环检测。移到根目录（ParentID == nil）不可能成环，跳过。
		// 下面这个判断背后的 store 方法在 repository/nodes.go 里，留给你实现。
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

// Delete 弱删节点：删文件夹时**连整棵子树一起**（D2 定的语义）。
//
// 一条递归 CTE 在数据库里标记完整棵树，文件就是"只有自己一个节点的子树"，
// 所以文件和文件夹共用同一条路径。
//
// 删完再回收内容（D3 的引用计数）：这次删除可能让某些 blob "最后一个引用消失"，
// 那才是真正该从磁盘上删掉的时候。回收逻辑在 store.ReclaimOrphanBlobs。
//
// 顺序和容错是刻意的：先弱删（这一步成功 = 用户看到的删除已经生效、已提交），
// 再回收。所以回收失败**不把整个请求判成失败** —— 那会让客户端以为没删掉，
// 而实际语义只是"内容多留了一会儿"（可重试、不违反任何承诺）。失败只留日志。
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
			// DB 行已删，对象没删掉 → 只是漏在盘上（下次覆盖同一 hash 会重新写一份新的），
			// 不影响任何读路径。
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
	Content io.ReadSeekCloser // 必须可 Seek：http.ServeContent 靠它免费拿到 Range 支持
}

// OpenDownload 打开一个文件供下载。所有权校验靠 GetNode 的 owner_id 条件完成。
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

// ZipDownload 是一次"文件夹打包下载"。
type ZipDownload struct {
	Name    string        // 建议的文件名（含 .zip）
	Content io.ReadCloser // 边遍历边产生的 zip 字节流
}

// OpenZip 打开一个文件夹的打包下载（P8）。
//
// 分两段，顺序是刻意的：
//  1. **同步**做能翻成 404/400 的校验 —— 节点存在、属于调用者、且是目录。这一段失败时
//     一个字节都还没写，所以能给客户端一个干净的 JSON 错误。
//  2. 遍历与打包丢进 goroutine，经 io.Pipe 流给 handler。一旦开始吐字节就改不了状态码，
//     中途出错只能 CloseWithError 让读端拿到错误（handler 记日志、断连）。
//
// 遍历那一段（writeZip）留给你实现 —— 契约见它的注释。
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
		// 出错时**不要**再 zw.Close()：那会吐出一个"空但合法"的 zip，
		// 让读端以为成功。直接把错误交给读端。
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

// writeZip 把 root 的整棵子树写进 zw。这是 P8 的核心，留给你实现。
//
// 契约（internal/service/zip_test.go 逐条验）：
//  1. 顶层前缀是 root.Name + "/"：root=docs、里面 a.txt → 条目名 "docs/a.txt"。
//  2. 条目名一律正斜杠、**不带前导 '/'**，目录项以 '/' 结尾。
//     （带前导 '/' 的条目 Windows 资源管理器会当成绝对路径，直接打不开压缩包。）
//  3. 空目录要显式写一条（名字以 '/' 结尾），否则解压后消失。
//  4. 文件内容用 f.storage.Open 流式拷进条目，**别整个读进内存**（判据同 D1：看进程 RSS）。
//  5. 遍历里 ctx 一取消就尽快返回（客户端断开时别接着读盘）。
//  6. 只走自己的树：向下扩展时每一步都带 owner_id 与 deleted_at IS NULL。
//     工具：f.store.ListChildren(ctx, ownerID, parentID)。
//  7. 已压缩的扩展名（.jpg .png .gif .zip .mp4 .mp3 …）用 zip.Store，其余 zip.Deflate，省 CPU。
//
// 出错就 return err；OpenZip 会把它带给读端。
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
					Method:   getCompressionMethod(child.Name), // 契约 7
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

// countingReader 数实际读过去的字节数 —— 上传前拿不到文件大小，
// 真实大小只能边读边数（也顺便当作 blob.size 的事实来源）。
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

// cleanHash 校验客户端报上来的 SHA-256：hex、32 字节，大小写都收（统一转小写，
// 因为 blobs.content_hash 存的就是十六进制小写）。
func cleanHash(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(raw))
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != sha256.Size {
		return "", invalid("hash 必须是 64 位十六进制的 SHA-256")
	}
	return h, nil
}

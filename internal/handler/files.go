package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Miraitowa-kawayi/netdisk/internal/httpx"
	"github.com/Miraitowa-kawayi/netdisk/internal/service"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// maxFieldBytes 限单个普通表单字段（parent_id / name）的大小。
// 文件本体不走这条路 —— 它是边收边写，不做任何缓冲。
const maxFieldBytes = 64 << 10

// uploadFile 处理 multipart/form-data 上传。
//
// 关键点：用 r.MultipartReader() 而不是 r.ParseMultipartForm()。
// 后者要先读完整个请求体（小文件进内存、大文件进临时目录）才交给业务层，
// 内存/磁盘都会随文件大小涨；MultipartReader 让我们在"还在收"的时候就把字节写下去。
//
// 约定：普通字段（parent_id / name）要出现在 file 部分之前 —— multipart 是顺序流，
// 读到 file 时来不及回头再取字段。也接受 ?parent_id=&name= 走 query，省掉这个顺序约束。
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("需要 multipart/form-data 请求体").With(err))
		return
	}

	parentID, err := parseParentID(r.URL.Query().Get("parent_id"))
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(err.Error()))
		return
	}
	name := r.URL.Query().Get("name")

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("multipart 请求体读不下去").With(err))
			return
		}

		// 带文件名的部分就是文件本体；字段部分先读掉。
		if part.FileName() == "" && part.FormName() != "file" {
			if err := s.consumeField(part, &parentID, &name); err != nil {
				_ = part.Close()
				httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(err.Error()))
				return
			}
			_ = part.Close()
			continue
		}

		// name 字段优先；没给就用 multipart 里的文件名（service 会剥掉路径、校验非空）。
		uploadName := name
		if uploadName == "" {
			uploadName = part.FileName()
		}

		node, err := s.deps.Files.Upload(r.Context(), service.UploadInput{
			OwnerID:  uid,
			ParentID: parentID,
			Name:     uploadName,
			SizeHint: -1, // multipart 分片拿不到单独的大小；真实大小由 service 边读边数
			Body:     part,
		})
		_ = part.Close()
		if err != nil {
			s.failService(w, r, err)
			return
		}

		drainParts(mr) // 读干净剩余部分，保持连接可复用（keep-alive）
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{"node": node})
		return
	}

	httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(`缺少 "file" 部分`))
}

// consumeField 读一个普通字段，值写回 parentID / name。
func (s *Server) consumeField(part *multipart.Part, parentID **uuid.UUID, name *string) error {
	key := part.FormName()
	if key != "parent_id" && key != "name" {
		_, _ = io.Copy(io.Discard, part) // 不认识的字段：读掉，保持流位置
		return nil
	}

	raw, err := io.ReadAll(io.LimitReader(part, maxFieldBytes))
	if err != nil {
		return fmt.Errorf("读取表单字段 %s 失败", key)
	}
	value := strings.TrimSpace(string(raw))

	if key == "name" {
		*name = value
		return nil
	}
	id, err := parseParentID(value)
	if err != nil {
		return err
	}
	*parentID = id
	return nil
}

func drainParts(mr *multipart.Reader) {
	for {
		part, err := mr.NextPart()
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, part)
		_ = part.Close()
	}
}

// parseParentID：空串与 "root" 都表示根目录（parent_id IS NULL）。
func parseParentID(raw string) (*uuid.UUID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "root" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, errors.New("parent_id 不是合法的 UUID")
	}
	return &id, nil
}

type fileItem struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	IsDir       bool      `json:"is_dir"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	DownloadURL string    `json:"download_url,omitempty"`
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	parentID, err := parseParentID(r.URL.Query().Get("parent_id"))
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(err.Error()))
		return
	}

	nodes, err := s.deps.Files.List(r.Context(), uid, parentID)
	if err != nil {
		s.failService(w, r, err)
		return
	}

	items := make([]fileItem, 0, len(nodes))
	for _, n := range nodes {
		item := fileItem{
			ID:        n.ID,
			Name:      n.Name,
			IsDir:     n.IsDir,
			Size:      n.Size,
			CreatedAt: n.CreatedAt,
			UpdatedAt: n.UpdatedAt,
		}
		if !n.IsDir {
			item.DownloadURL = "/api/v1/files/" + n.ID.String() + "/download"
		}
		items = append(items, item)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"files": items})
}

func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.NotFound("no such file"))
		return
	}

	dl, err := s.deps.Files.OpenDownload(r.Context(), uid, id)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	defer dl.Content.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition(dl.Name))

	// ServeContent 认 io.ReadSeeker，于是 Range / If-Range / 206 全是白送的，
	// 不用自己解析 "bytes=3000000-5000000"（D4 断点续传就站在这里）。
	http.ServeContent(w, r, dl.Name, dl.ModTime, dl.Content)
}

// createDirRequest 是 POST /files/dirs 的请求体。
// parent_id 缺省、空串、null 都表示根目录（JSON 的 null 解到 string 会保持零值）。
type createDirRequest struct {
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
}

// createDir 新建文件夹。目录没有内容流，所以走普通 JSON 而不是 multipart。
func (s *Server) createDir(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	var req createDirRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("请求体不是合法的 JSON").With(err))
		return
	}
	parentID, err := parseParentID(req.ParentID)
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(err.Error()))
		return
	}

	node, err := s.deps.Files.CreateDir(r.Context(), uid, parentID, req.Name)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"node": node})
}

// instantUploadRequest 是 POST /files/instant 的请求体。
// hash 是客户端对**本地文件**算好的 SHA-256（hex）；服务端命中就一个字节都不收。
type instantUploadRequest struct {
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
	Hash     string `json:"hash"`
	Size     int64  `json:"size"`
}

// instantUpload 秒传：客户端报 hash，服务端已有这份内容就直接建节点（只插一行 nodes）。
//
// 命中要求内容**已经**在库里（别人传过或自己传过）；没有 → 404，客户端回到
// 普通 multipart 上传路径把字节传一遍。响应里带 "instant": true 是给演示/客户端
// 一个明确的信号："这次真的一个字节都没传"。
func (s *Server) instantUpload(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	var req instantUploadRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("请求体不是合法的 JSON").With(err))
		return
	}
	parentID, err := parseParentID(req.ParentID)
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(err.Error()))
		return
	}

	node, err := s.deps.Files.InstantUpload(r.Context(), service.InstantInput{
		OwnerID:  uid,
		ParentID: parentID,
		Name:     req.Name,
		Hash:     req.Hash,
		Size:     req.Size,
	})
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"node": node, "instant": true})
}

// patchNodeRequest 是 PATCH /files/{id} 的请求体。
//
// ParentID 用 json.RawMessage 是为了区分 JSON 里三种不同的情况 ——
// 换成 *uuid.UUID 就区分不出来了：
//
//	键不存在    → nil                        （不改父目录）
//	键是 null   → []byte("null")             （移回根目录）
//	键是个 uuid → 那个 uuid                   （移到该目录下）
type patchNodeRequest struct {
	Name     *string         `json:"name"`
	ParentID json.RawMessage `json:"parent_id"`
}

// patchFile 改名和/或移动。一个端点两种用途：给了 name 就是改名，
// 给了 parent_id 就是移动，两个都给就一起做。
func (s *Server) patchFile(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.NotFound("no such file"))
		return
	}

	var req patchNodeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("请求体不是合法的 JSON").With(err))
		return
	}
	if req.Name == nil && req.ParentID == nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("至少要给一个 name 或 parent_id"))
		return
	}

	in := service.UpdateInput{Name: req.Name}
	if req.ParentID != nil {
		in.SetParent = true
		// null（而不是缺省）才是"移回根目录"。
		if !bytes.Equal(bytes.TrimSpace(req.ParentID), []byte("null")) {
			var raw string
			if err := json.Unmarshal(req.ParentID, &raw); err != nil {
				httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("parent_id 必须是字符串或 null"))
				return
			}
			p, err := parseParentID(raw)
			if err != nil {
				httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(err.Error()))
				return
			}
			in.ParentID = p
		}
	}

	node, err := s.deps.Files.Update(r.Context(), uid, id, in)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"node": node})
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.NotFound("no such file"))
		return
	}

	if err := s.deps.Files.Delete(r.Context(), uid, id); err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// contentDisposition 让中文文件名在各浏览器里都不乱码：
// filename= 给老客户端（ASCII 化），filename*= 给现代客户端（RFC 5987）。
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '-'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(name))
}

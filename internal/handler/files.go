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

// maxFieldBytes 限制普通表单字段（parent_id / name）的大小，文件本体不经过此限制。
const maxFieldBytes = 64 << 10

// uploadFile 处理 multipart/form-data 上传。
// 用 r.MultipartReader() 边收边写，不用 ParseMultipartForm()，避免整个请求体进内存或临时文件。
// 普通字段（parent_id / name）须在 file 部分之前，或用 query 参数绕开该顺序约束。
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

		// 带文件名的部分即文件本体，其余为表单字段。
		if part.FileName() == "" && part.FormName() != "file" {
			if err := s.consumeField(part, &parentID, &name); err != nil {
				_ = part.Close()
				httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(err.Error()))
				return
			}
			_ = part.Close()
			continue
		}

		// name 字段优先，缺省时用 multipart 文件名。
		uploadName := name
		if uploadName == "" {
			uploadName = part.FileName()
		}

		node, err := s.deps.Files.Upload(r.Context(), service.UploadInput{
			OwnerID:  uid,
			ParentID: parentID,
			Name:     uploadName,
			SizeHint: -1, // -1：multipart 拿不到大小，真实大小由 service 边读边数
			Body:     part,
		})
		_ = part.Close()
		if err != nil {
			s.failService(w, r, err)
			return
		}

		drainParts(mr) // 读完剩余部分，保持连接可复用
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{"node": node})
		return
	}

	httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(`缺少 "file" 部分`))
}

// consumeField 读一个普通字段，值写回 parentID / name。
func (s *Server) consumeField(part *multipart.Part, parentID **uuid.UUID, name *string) error {
	key := part.FormName()
	if key != "parent_id" && key != "name" {
		_, _ = io.Copy(io.Discard, part) // 不认识的字段读掉，保持流位置
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

// parseParentID 把空串或 "root" 解析为 nil（根目录）。
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

	// ServeContent 接受 io.ReadSeeker，Range / If-Range / 206 由它处理。
	http.ServeContent(w, r, dl.Name, dl.ModTime, dl.Content)
}

// downloadZip 把文件夹整棵子树打包成 zip 流式下发。
// zip 的中央目录在末尾，无法提供 Content-Length 或 Range，因此边遍历边输出。
func (s *Server) downloadZip(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.NotFound("no such folder"))
		return
	}

	z, err := s.deps.Files.OpenZip(r.Context(), uid, id)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	// 读端提前退出时 Close，让写端的 pw.Write 立刻失败，避免遍历 goroutine 阻塞在管道上。
	defer z.Content.Close()

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(z.Name))
	if _, err := io.Copy(w, z.Content); err != nil {
		// 响应头已发出，状态码无法修改，只记日志。
		s.deps.Logger.Warn("stream folder zip", "node_id", id, "error", err)
	}
}

// createDirRequest 是 POST /files/dirs 的请求体，parent_id 缺省或为空表示根目录。
type createDirRequest struct {
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
}

// createDir 新建文件夹。
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

// instantUploadRequest 是 POST /files/instant 的请求体，hash 为客户端上报的 SHA-256（hex）。
type instantUploadRequest struct {
	Name     string `json:"name"`
	ParentID string `json:"parent_id"`
	Hash     string `json:"hash"`
	Size     int64  `json:"size"`
}

// instantUpload 秒传：内容已在库中时直接建节点，不接收字节。
// 未命中返回 404，客户端改用普通 multipart 上传；响应带 "instant": true。
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
// ParentID 用 json.RawMessage 以区分三种输入：
//
//	键不存在  → 不改父目录
//	键是 null → 移回根目录
//	键是 uuid → 移到该目录下
type patchNodeRequest struct {
	Name     *string         `json:"name"`
	ParentID json.RawMessage `json:"parent_id"`
}

// patchFile 改名和/或移动：给 name 即改名，给 parent_id 即移动。
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
		// 值为 null 才表示移回根目录。
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

// contentDisposition 同时给出 filename= 与 filename*=（RFC 5987），避免中文文件名乱码。
func contentDisposition(name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 || r == '"' || r == '\\' {
			return '-'
		}
		return r
	}, name)
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(name))
}

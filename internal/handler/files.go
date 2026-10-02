package handler

import (
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

type renameRequest struct {
	Name string `json:"name"`
}

func (s *Server) renameFile(w http.ResponseWriter, r *http.Request) {
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

	var req renameRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("请求体不是合法的 JSON").With(err))
		return
	}

	node, err := s.deps.Files.Rename(r.Context(), uid, id, req.Name)
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

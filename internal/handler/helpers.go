package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Miraitowa-kawayi/netdisk/internal/httpx"
	"github.com/Miraitowa-kawayi/netdisk/internal/middleware"
	"github.com/Miraitowa-kawayi/netdisk/internal/service"
	"github.com/google/uuid"
)

// maxJSONBody 限制 JSON 请求体；上传走 multipart 流式路径，不受此限制。
const maxJSONBody = 64 << 10

// decodeJSON 解请求体：MaxBytesReader 限制大小，DisallowUnknownFields 让字段拼写错误立即报错。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// userID 取当前登录用户，由 RequireAuth 写入。
func userID(r *http.Request) (uuid.UUID, bool) {
	return middleware.UserIDFromContext(r.Context())
}

// failService 把 service 层错误映射为 HTTP 响应，状态码映射集中在此。
func (s *Server) failService(w http.ResponseWriter, r *http.Request, err error) {
	var v *service.ValidationError

	switch {
	case errors.As(err, &v):
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(v.Msg))
	case errors.Is(err, service.ErrUsernameTaken), errors.Is(err, service.ErrNameConflict):
		httpx.Fail(w, r, s.deps.Logger, httpx.Conflict(err.Error()))
	case errors.Is(err, service.ErrCycle):
		// 移动成环是资源状态冲突，返回 409。
		httpx.Fail(w, r, s.deps.Logger, httpx.Conflict(err.Error()))
	case errors.Is(err, service.ErrUploadNotPending), errors.Is(err, service.ErrUploadIncomplete):
		// 会话状态或分片不齐属于资源状态冲突，返回 409。
		httpx.Fail(w, r, s.deps.Logger, httpx.Conflict(err.Error()))
	case errors.Is(err, service.ErrInvalidCredentials):
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized(err.Error()))
	case errors.Is(err, service.ErrNotFound):
		httpx.Fail(w, r, s.deps.Logger, httpx.NotFound(err.Error()))
	case errors.Is(err, service.ErrContentNotStored):
		// 秒传未命中：服务端没有该内容，返回 404。
		httpx.Fail(w, r, s.deps.Logger, httpx.NotFound(err.Error()))
	case errors.Is(err, service.ErrNotAFile), errors.Is(err, service.ErrNotADirectory):
		// 目标类型不匹配（文件夹当文件、文件当文件夹）返回 400。
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(err.Error()))
	default:
		// 未识别的错误按 500 处理，细节只写日志。
		httpx.Fail(w, r, s.deps.Logger, err)
	}
}

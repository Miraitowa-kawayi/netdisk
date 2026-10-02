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

// maxJSONBody 限制 JSON 请求体。上传走 multipart 流式路径，不受这个值影响。
const maxJSONBody = 64 << 10

// decodeJSON 解请求体。用 MaxBytesReader 挡住超大 body，DisallowUnknownFields
// 让客户端的拼写错误立刻暴露，而不是被静默忽略。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// userID 取当前登录用户。走到这里说明 RequireAuth 已经放行过。
func userID(r *http.Request) (uuid.UUID, bool) {
	return middleware.UserIDFromContext(r.Context())
}

// failService 把业务层的错误翻译成 HTTP 响应。这是"service 不 import net/http"
// 的代价：所有状态码的映射集中在这一个函数里，一目了然。
func (s *Server) failService(w http.ResponseWriter, r *http.Request, err error) {
	var v *service.ValidationError

	switch {
	case errors.As(err, &v):
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(v.Msg))
	case errors.Is(err, service.ErrUsernameTaken), errors.Is(err, service.ErrNameConflict):
		httpx.Fail(w, r, s.deps.Logger, httpx.Conflict(err.Error()))
	case errors.Is(err, service.ErrCycle):
		// 移动成环：请求与当前资源状态冲突，不是参数格式错 —— 所以是 409 不是 400。
		httpx.Fail(w, r, s.deps.Logger, httpx.Conflict(err.Error()))
	case errors.Is(err, service.ErrInvalidCredentials):
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized(err.Error()))
	case errors.Is(err, service.ErrNotFound):
		httpx.Fail(w, r, s.deps.Logger, httpx.NotFound(err.Error()))
	case errors.Is(err, service.ErrNotAFile):
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(err.Error()))
	default:
		// 未识别的错误：httpx.Fail 会按 500 处理并把细节写进日志，不泄露给客户端
		httpx.Fail(w, r, s.deps.Logger, err)
	}
}

// Package httpx 统一 HTTP 层的响应格式与错误码。
//
// 约定：所有错误响应都是 {"error":{"code":"...","message":"..."}}；
// code 是对外稳定的机器可读标识，HTTP 状态码只作粗粒度提示。
package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// 对外错误码。
const (
	CodeInvalidRequest = "invalid_request"
	CodeUnauthorized   = "unauthorized"
	CodeForbidden      = "forbidden"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeNotReady       = "not_ready"
	CodeInternal       = "internal_error"
)

// Error 是贯穿各层的应用错误。Err 保存内部原因，只写日志，绝不返回给客户端。
type Error struct {
	Status  int
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Code + ": " + e.Message
	}
	return e.Code + ": " + e.Message + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// With 把内部错误挂上来，链式使用：httpx.NotFound("x").With(err)
func (e *Error) With(err error) *Error {
	e.Err = err
	return e
}

// New 构造一个应用错误。
func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func Invalid(message string) *Error {
	return New(http.StatusBadRequest, CodeInvalidRequest, message)
}

func Unauthorized(message string) *Error {
	return New(http.StatusUnauthorized, CodeUnauthorized, message)
}

func Forbidden(message string) *Error {
	return New(http.StatusForbidden, CodeForbidden, message)
}

func NotFound(message string) *Error {
	return New(http.StatusNotFound, CodeNotFound, message)
}

func Conflict(message string) *Error {
	return New(http.StatusConflict, CodeConflict, message)
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// WriteJSON 写一个 JSON 响应。v 为 nil 时只写状态码。
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write json response", "error", err)
	}
}

// Fail 把任意错误翻译成 JSON 错误响应。
// 非 *Error 一律按 500 处理，并且只把细节写进日志 —— 不泄露内部状态给客户端。
func Fail(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	appErr := New(http.StatusInternalServerError, CodeInternal, "internal error")
	if !errors.As(err, &appErr) {
		logger.Error("unhandled error", "error", err, "method", r.Method, "path", r.URL.Path)
	} else if appErr.Status >= http.StatusInternalServerError {
		logger.Error("server error", "code", appErr.Code, "error", appErr, "path", r.URL.Path)
	}

	body := errorBody{}
	body.Error.Code = appErr.Code
	body.Error.Message = appErr.Message
	WriteJSON(w, appErr.Status, body)
}

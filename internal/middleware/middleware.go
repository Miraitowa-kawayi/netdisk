// Package middleware 是 HTTP 中间件：请求日志、panic 兜底。
package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/Miraitowa-kawayi/netdisk/internal/httpx"
	chimw "github.com/go-chi/chi/v5/middleware"
)

// RequestLogger 记录每个请求的方法、路径、状态码、字节数与耗时，需先挂 chimw.RequestID。
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			logger.Info("http",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration", time.Since(start).String(),
				"request_id", chimw.GetReqID(r.Context()),
			)
		})
	}
}

// Recoverer 捕获 handler 里的 panic，返回统一错误体而不是标准库的纯文本 500。
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// ErrAbortHandler 表示主动中止连接，必须原样抛出。
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				logger.Error("panic recovered",
					"panic", rec,
					"method", r.Method,
					"path", r.URL.Path,
					"request_id", chimw.GetReqID(r.Context()),
				)
				httpx.Fail(w, r, logger, httpx.New(http.StatusInternalServerError, httpx.CodeInternal, "internal error"))
			}()

			next.ServeHTTP(w, r)
		})
	}
}

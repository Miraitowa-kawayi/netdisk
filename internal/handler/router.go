// Package handler 是 HTTP 层：路由、参数校验、状态码，不做业务规则。
package handler

import (
	"log/slog"
	"net/http"

	"github.com/Miraitowa-kawayi/netdisk/internal/config"
	"github.com/Miraitowa-kawayi/netdisk/internal/httpx"
	"github.com/Miraitowa-kawayi/netdisk/internal/middleware"
	"github.com/Miraitowa-kawayi/netdisk/internal/repository"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
)

// Deps 是 HTTP 层需要的依赖。
type Deps struct {
	Cfg    *config.Config
	Logger *slog.Logger
	Store  *repository.Store
}

// Server 持有依赖，各 handler 是它的方法。
type Server struct {
	deps Deps
}

// NewRouter 组装中间件与路由。
func NewRouter(deps Deps) http.Handler {
	s := &Server{deps: deps}

	r := chi.NewRouter()
	r.Use(chimw.RequestID)
	r.Use(middleware.RequestLogger(deps.Logger))
	r.Use(middleware.Recoverer(deps.Logger))
	// 刻意不加 chi 的 Timeout / RequestSize 中间件：上传是大体积长连接，
	// 统一限时会让大文件必然失败。超时策略放在单条路由上按需设置。

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Fail(w, r, deps.Logger, httpx.NotFound("no such endpoint"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusMethodNotAllowed
		httpx.Fail(w, r, deps.Logger, httpx.New(code, httpx.CodeInvalidRequest, "method not allowed"))
	})

	// 存活与就绪分开：healthz 不碰依赖，readyz 要求数据库可达。
	r.Get("/healthz", s.healthz)
	r.Get("/readyz", s.readyz)

	// /api/v1 分组从 D1 开始建：/auth、/files、/folders、/shares。
	return r
}

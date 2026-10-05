// Package handler 是 HTTP 层：路由、参数校验、状态码，不做业务规则。
package handler

import (
	"log/slog"
	"net/http"

	"github.com/Miraitowa-kawayi/netdisk/internal/config"
	"github.com/Miraitowa-kawayi/netdisk/internal/httpx"
	"github.com/Miraitowa-kawayi/netdisk/internal/middleware"
	"github.com/Miraitowa-kawayi/netdisk/internal/repository"
	"github.com/Miraitowa-kawayi/netdisk/internal/service"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
)

// Deps 是 HTTP 层需要的依赖。
type Deps struct {
	Cfg     *config.Config
	Logger  *slog.Logger
	Store   *repository.Store
	Auth    *service.Auth
	Files   *service.Files
	Uploads *service.Uploads
	Shares  *service.Shares
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

	// /api/v1：auth 是公开的（注册/登录），其余全部要 Bearer token。
	requireAuth := middleware.RequireAuth(deps.Logger, deps.Auth.Tokens())

	r.Route("/api/v1", func(r chi.Router) {
		r.Route("/auth", func(r chi.Router) {
			r.Post("/register", s.authRegister)
			r.Post("/login", s.authLogin)
			r.Group(func(r chi.Router) {
				r.Use(requireAuth)
				r.Get("/me", s.authMe)
			})
		})

		r.Route("/files", func(r chi.Router) {
			r.Use(requireAuth)
			r.Post("/", s.uploadFile)               // multipart，全程流式
			r.Post("/dirs", s.createDir)            // 新建文件夹（JSON）
			r.Post("/instant", s.instantUpload)     // 秒传：报 hash，命中就不收字节（JSON）
			r.Get("/", s.listFiles)                 // ?parent_id=<uuid>|root
			r.Get("/{id}/download", s.downloadFile) // ServeContent → Range/206 白送
			r.Get("/{id}/zip", s.downloadZip)       // P8：整棵子树打包成 zip，流式、无 Range
			r.Patch("/{id}", s.patchFile)           // 改名和/或移动，看给了哪个字段
			r.Delete("/{id}", s.deleteFile)         // 弱删；文件夹连整棵子树
		})

		// P7 断点续传：分片上传的状态机在 /uploads 下。
		r.Route("/uploads", func(r chi.Router) {
			r.Use(requireAuth)
			r.Post("/", s.createUpload)                     // 开会话：name / parent_id / total_size / chunk_size
			r.Get("/", s.listUploads)                       // 列 pending 会话（丢了 session id 时找回）
			r.Get("/{id}", s.uploadSessionStatus)           // 进度：已收到 / 还缺哪些片
			r.Put("/{id}/parts/{part_no}", s.putUploadPart) // 传一片（原始字节，流式）
			r.Post("/{id}/complete", s.completeUpload)      // 收尾：按序合并 + 建节点
			r.Delete("/{id}", s.abortUpload)                // 放弃：清理分片
		})

		// P4 分享：管理要登录；匿名访问走 /share/{token}，故意**不挂** requireAuth ——
		// 拿着 token 的人就是被授权的人（校验在 service 里做）。
		r.Route("/shares", func(r chi.Router) {
			r.Use(requireAuth)
			r.Post("/", s.createShare)       // 建：{node_id, expires_in_seconds?}
			r.Get("/", s.listShares)         // 列我建过的
			r.Delete("/{id}", s.deleteShare) // 撤销
		})
		r.Get("/share/{token}", s.resolveShare)              // 匿名打开（文件/文件夹元信息）
		r.Get("/share/{token}/download", s.downloadViaShare) // 匿名下载（?node_id= 指定子项）
	})

	return r
}

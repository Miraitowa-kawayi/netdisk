package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/Miraitowa-kawayi/netdisk/internal/httpx"
)

// healthz 只表示进程活着 —— 不碰任何依赖，这样数据库挂了也能探到进程本身的状态。
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz 表示"能对外服务"：依赖必须可用。注意别用它做保活探针，
// 否则数据库抖一下容器就会被重启。
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.deps.Store.Ping(ctx); err != nil {
		err := httpx.New(http.StatusServiceUnavailable, httpx.CodeNotReady, "database unavailable").With(err)
		httpx.Fail(w, r, s.deps.Logger, err)
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/Miraitowa-kawayi/netdisk/internal/httpx"
)

// healthz 只表示进程存活，不检查依赖。
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz 表示依赖可用、能对外服务；不要用它做保活探针。
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

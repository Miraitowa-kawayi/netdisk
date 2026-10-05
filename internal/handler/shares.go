package handler

import (
	"net/http"
	"time"

	"github.com/Miraitowa-kawayi/netdisk/internal/httpx"
	"github.com/Miraitowa-kawayi/netdisk/internal/service"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// createShareRequest 是 POST /shares 的请求体，expires_in_seconds 缺省或 0 表示永不过期。
type createShareRequest struct {
	NodeID           string `json:"node_id"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
}

// createShare 给自己的一个节点建分享链接。
func (s *Server) createShare(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	var req createShareRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("请求体不是合法的 JSON").With(err))
		return
	}
	nodeID, err := uuid.Parse(req.NodeID)
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("node_id 必须是 UUID"))
		return
	}
	if req.ExpiresInSeconds < 0 {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("expires_in_seconds 不能为负"))
		return
	}

	view, err := s.deps.Shares.Create(r.Context(), service.ShareInput{
		OwnerID:   uid,
		NodeID:    nodeID,
		ExpiresIn: time.Duration(req.ExpiresInSeconds) * time.Second,
	})
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"share": view.Share, "url": view.URL})
}

// listShares 列当前用户创建的分享。
func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	shares, err := s.deps.Shares.List(r.Context(), uid)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"shares": shares})
}

// deleteShare 撤销一条分享。
func (s *Server) deleteShare(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.NotFound("no such share"))
		return
	}
	if err := s.deps.Shares.Revoke(r.Context(), uid, id); err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

// resolveShare 匿名打开一条分享，返回目标节点元信息，目录时连直接子项一起返回。
func (s *Server) resolveShare(w http.ResponseWriter, r *http.Request) {
	view, err := s.deps.Shares.Resolve(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"node":     view.Node,
		"children": view.Children,
	})
}

// downloadViaShare 匿名下载；?node_id=<uuid> 可选，分享目录时指定下载的子项。
func (s *Server) downloadViaShare(w http.ResponseWriter, r *http.Request) {
	var target *uuid.UUID
	if raw := r.URL.Query().Get("node_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("node_id 必须是 UUID"))
			return
		}
		target = &id
	}

	dl, err := s.deps.Shares.OpenDownload(r.Context(), chi.URLParam(r, "token"), target)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	defer dl.Content.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition(dl.Name))
	http.ServeContent(w, r, dl.Name, dl.ModTime, dl.Content)
}

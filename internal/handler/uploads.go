package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Miraitowa-kawayi/netdisk/internal/httpx"
	"github.com/Miraitowa-kawayi/netdisk/internal/service"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// createUploadRequest 是 POST /uploads 的请求体：开一次分片上传的意图。
type createUploadRequest struct {
	Name      string `json:"name"`
	ParentID  string `json:"parent_id"`
	TotalSize int64  `json:"total_size"`
	ChunkSize int64  `json:"chunk_size"`
	Hash      string `json:"hash"` // 可选：客户端预申报整份 SHA-256
}

// createUpload 开一个分片会话，返回 session id + 分片数。
func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	var req createUploadRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("请求体不是合法的 JSON").With(err))
		return
	}
	parentID, err := parseParentID(req.ParentID)
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid(err.Error()))
		return
	}
	var contentHash *string
	if h := strings.ToLower(strings.TrimSpace(req.Hash)); h != "" {
		contentHash = &h
	}

	sess, err := s.deps.Uploads.Create(r.Context(), service.CreateInput{
		OwnerID:     uid,
		ParentID:    parentID,
		Name:        req.Name,
		TotalSize:   req.TotalSize,
		ChunkSize:   req.ChunkSize,
		ContentHash: contentHash,
	})
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"session": sess})
}

// listUploads 列当前用户还没收尾的会话（客户端丢了 session id 时用来找回）。
func (s *Server) listUploads(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}
	sessions, err := s.deps.Uploads.List(r.Context(), uid)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// uploadSessionStatus 返回会话进度：已收到 / 还缺哪些分片 —— 断点续传的"读状态"那一步。
func (s *Server) uploadSessionStatus(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}
	sessionID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.NotFound("no such upload session"))
		return
	}

	st, err := s.deps.Uploads.Status(r.Context(), uid, sessionID)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"session":        st.Session,
		"received_parts": st.ReceivedParts,
		"missing_parts":  st.MissingParts,
	})
}

// putUploadPart 收一个分片。请求体就是该片的原始字节（application/octet-stream），
// 全程流式写进存储 —— 不缓冲、不进内存。重复 PUT 同一 part_no 是幂等的（覆盖）。
func (s *Server) putUploadPart(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}
	sessionID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.NotFound("no such upload session"))
		return
	}
	partNo, err := strconv.Atoi(chi.URLParam(r, "part_no"))
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("part_no 必须是整数"))
		return
	}

	part, err := s.deps.Uploads.PutPart(r.Context(), uid, sessionID, partNo, r.Body)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"part": part})
}

// completeUpload 收尾：服务端把分片按序拼成完整文件并建节点。
func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}
	sessionID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.NotFound("no such upload session"))
		return
	}

	node, err := s.deps.Uploads.Complete(r.Context(), uid, sessionID)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"node": node})
}

// abortUpload 放弃会话并清掉已收到的分片。
func (s *Server) abortUpload(w http.ResponseWriter, r *http.Request) {
	uid, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}
	sessionID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.NotFound("no such upload session"))
		return
	}

	if err := s.deps.Uploads.Abort(r.Context(), uid, sessionID); err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusNoContent, nil)
}

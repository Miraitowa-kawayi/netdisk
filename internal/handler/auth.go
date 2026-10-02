package handler

import (
	"net/http"

	"github.com/Miraitowa-kawayi/netdisk/internal/httpx"
)

type registerRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Nickname string `json:"nickname"`
}

func (s *Server) authRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("请求体不是合法的 JSON").With(err))
		return
	}

	user, err := s.deps.Auth.Register(r.Context(), req.Username, req.Password, req.Nickname)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"user": user})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, s.deps.Logger, httpx.Invalid("请求体不是合法的 JSON").With(err))
		return
	}

	user, token, expiresAt, err := s.deps.Auth.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"token_type": "Bearer",
		"expires_at": expiresAt,
		"user":       user,
	})
}

func (s *Server) authMe(w http.ResponseWriter, r *http.Request) {
	id, ok := userID(r)
	if !ok {
		httpx.Fail(w, r, s.deps.Logger, httpx.Unauthorized("authentication required"))
		return
	}

	user, err := s.deps.Auth.Me(r.Context(), id)
	if err != nil {
		s.failService(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"user": user})
}

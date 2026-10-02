package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Miraitowa-kawayi/netdisk/internal/httpx"
	"github.com/google/uuid"
)

// TokenParser 由 service.Tokens 满足。middleware 只依赖这个窄接口，
// 于是它不必 import 业务包（也避免以后绕出循环依赖）。
type TokenParser interface {
	Parse(raw string) (uuid.UUID, error)
}

type ctxKey int

const userIDKey ctxKey = iota

// RequireAuth 校验 Authorization: Bearer <token>，通过后把用户 id 放进 context。
// 失败一律 401，且不区分"没带 token / 签名不对 / 过期"—— 不给爆破者提示。
func RequireAuth(logger *slog.Logger, tp TokenParser) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				httpx.Fail(w, r, logger, httpx.Unauthorized("missing or malformed Authorization header"))
				return
			}
			id, err := tp.Parse(raw)
			if err != nil {
				httpx.Fail(w, r, logger, httpx.Unauthorized("invalid or expired token"))
				return
			}
			ctx := context.WithValue(r.Context(), userIDKey, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// UserIDFromContext 取出 RequireAuth 放进去的用户 id。
func UserIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userIDKey).(uuid.UUID)
	return id, ok && id != uuid.Nil
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	return token, token != ""
}

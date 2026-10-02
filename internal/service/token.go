package service

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Tokens 负责签发与校验 JWT（HS256）。
// 它只需要一个 Parse 方法就能满足 middleware 的窄接口，所以 middleware 不必认识 service 包。
type Tokens struct {
	secret []byte
	ttl    time.Duration
}

func NewTokens(secret string, ttl time.Duration) *Tokens {
	return &Tokens{secret: []byte(secret), ttl: ttl}
}

// Issue 给用户签发 token，同时返回过期时间，好让客户端知道什么时候该重新登录。
func (t *Tokens) Issue(userID uuid.UUID) (string, time.Time, error) {
	now := time.Now()
	expiresAt := now.Add(t.ttl)

	claims := jwt.RegisteredClaims{
		Subject:   userID.String(),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt), // Parse 时会自动校验它
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, err
	}
	return signed, expiresAt, nil
}

// Parse 校验签名与过期时间，返回用户 id。
//
// WithValidMethods 不能省：不限定算法的话，"alg: none" 或 RS256/HS256 混淆这类攻击就有空子。
func (t *Tokens) Parse(raw string) (uuid.UUID, error) {
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(raw, &claims,
		func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return uuid.Nil, err
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, errors.New("token subject is not a uuid")
	}
	return id, nil
}

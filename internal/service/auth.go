package service

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/Miraitowa-kawayi/netdisk/internal/model"
	"github.com/Miraitowa-kawayi/netdisk/internal/repository"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

const (
	minUsernameLen = 3
	maxUsernameLen = 32
	minPasswordLen = 8
	maxPasswordLen = 72 // bcrypt 只看前 72 字节
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// dummyHash 供"用户不存在"路径也走一次 bcrypt，使两条失败路径耗时一致。
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("netdisk-dummy-password"), bcrypt.DefaultCost)

// Auth 负责注册、登录与当前用户查询。
type Auth struct {
	store  *repository.Store
	tokens *Tokens
}

func NewAuth(store *repository.Store, tokens *Tokens) *Auth {
	return &Auth{store: store, tokens: tokens}
}

// Tokens 暴露给 router 装鉴权中间件。
func (a *Auth) Tokens() *Tokens { return a.tokens }

// Register 建用户。用户名已存在返回 ErrUsernameTaken。
func (a *Auth) Register(ctx context.Context, username, password, nickname string) (model.User, error) {
	username = strings.TrimSpace(username)
	if err := validateCredentials(username, password); err != nil {
		return model.User{}, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return model.User{}, err
	}

	user, err := a.store.CreateUser(ctx, username, string(hash), strings.TrimSpace(nickname))
	if errors.Is(err, repository.ErrUniqueViolation) {
		return model.User{}, ErrUsernameTaken
	}
	if err != nil {
		return model.User{}, err
	}
	return user, nil
}

// Login 校验凭证并签发 token。用户名不存在与密码错误返回同一个错误。
func (a *Auth) Login(ctx context.Context, username, password string) (model.User, string, time.Time, error) {
	user, err := a.store.GetUserByUsername(ctx, strings.TrimSpace(username))
	if errors.Is(err, repository.ErrNotFound) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password)) // 拉平两条路径的耗时
		return model.User{}, "", time.Time{}, ErrInvalidCredentials
	}
	if err != nil {
		return model.User{}, "", time.Time{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		return model.User{}, "", time.Time{}, ErrInvalidCredentials
	}

	token, expiresAt, err := a.tokens.Issue(user.ID)
	if err != nil {
		return model.User{}, "", time.Time{}, err
	}
	return user, token, expiresAt, nil
}

// Me 取当前 token 对应的用户。
func (a *Auth) Me(ctx context.Context, userID uuid.UUID) (model.User, error) {
	user, err := a.store.GetUserByID(ctx, userID)
	if errors.Is(err, repository.ErrNotFound) {
		// token 有效但用户已被删：按未认证处理
		return model.User{}, ErrInvalidCredentials
	}
	return user, err
}

func validateCredentials(username, password string) error {
	if l := len(username); l < minUsernameLen || l > maxUsernameLen {
		return invalid("用户名长度必须在 %d–%d 之间", minUsernameLen, maxUsernameLen)
	}
	if !usernamePattern.MatchString(username) {
		return invalid("用户名只能包含字母、数字、下划线和连字符")
	}
	if l := len(password); l < minPasswordLen || l > maxPasswordLen {
		return invalid("密码长度必须在 %d–%d 之间", minPasswordLen, maxPasswordLen)
	}
	return nil
}

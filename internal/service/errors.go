package service

import (
	"errors"
	"fmt"
)

// 业务错误。这一层刻意不 import net/http —— 翻译成状态码是 handler 的事。
var (
	// ErrNotFound 覆盖"不存在"和"不属于你"两种情况（对外不区分）。
	ErrNotFound = errors.New("not found")
	// ErrUsernameTaken 注册时用户名被占用。
	ErrUsernameTaken = errors.New("username already taken")
	// ErrInvalidCredentials 用户不存在与密码错误共用这一个，不给爆破者额外信息。
	ErrInvalidCredentials = errors.New("invalid username or password")
	// ErrNameConflict 同一目录下已有同名文件/文件夹。
	ErrNameConflict = errors.New("another entry with the same name already exists here")
	// ErrNotAFile 下载目标是个文件夹。
	ErrNotAFile = errors.New("target is a folder, not a file")
)

// ValidationError 携带一句可以直接给用户看的说明（参数不合法，不是内部故障）。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

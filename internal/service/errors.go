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
	// ErrCycle 移动会让文件树成环：把文件夹移进它自己或它自己的子孙里。
	ErrCycle = errors.New("cannot move a folder into itself or its own subdirectory")
	// ErrContentNotStored 秒传时服务端没有这个 hash 的内容 —— 客户端得老实传一遍。
	ErrContentNotStored = errors.New("no content with that hash is stored here")
	// ErrUploadNotPending 分片会话不在 pending 状态（已完成或已放弃）—— 不能再传分片/收尾。
	ErrUploadNotPending = errors.New("upload session is no longer pending")
	// ErrUploadIncomplete 收尾时会话还缺分片 —— 客户端要先把缺的补上再收尾。
	ErrUploadIncomplete = errors.New("upload session is missing one or more parts")
)

// ValidationError 携带一句可以直接给用户看的说明（参数不合法，不是内部故障）。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

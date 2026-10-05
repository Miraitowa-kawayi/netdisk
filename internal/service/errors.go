package service

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound 同时表示"不存在"和"不属于调用者"。
	ErrNotFound = errors.New("not found")
	// ErrUsernameTaken 注册时用户名已被占用。
	ErrUsernameTaken = errors.New("username already taken")
	// ErrInvalidCredentials 表示用户不存在或密码错误。
	ErrInvalidCredentials = errors.New("invalid username or password")
	// ErrNameConflict 同一目录下已有同名文件或文件夹。
	ErrNameConflict = errors.New("another entry with the same name already exists here")
	// ErrNotAFile 下载目标是个文件夹。
	ErrNotAFile = errors.New("target is a folder, not a file")
	// ErrNotADirectory 打包下载的目标是文件而不是文件夹。
	ErrNotADirectory = errors.New("target is a file, not a folder")
	// ErrCycle 移动会让文件夹进入自身或其后代。
	ErrCycle = errors.New("cannot move a folder into itself or its own subdirectory")
	// ErrContentNotStored 秒传时服务端没有该 hash 的内容。
	ErrContentNotStored = errors.New("no content with that hash is stored here")
	// ErrUploadNotPending 分片会话已不在 pending 状态。
	ErrUploadNotPending = errors.New("upload session is no longer pending")
	// ErrUploadIncomplete 收尾时会话仍缺分片。
	ErrUploadIncomplete = errors.New("upload session is missing one or more parts")
)

// ValidationError 是参数校验错误，Msg 可直接展示给用户。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

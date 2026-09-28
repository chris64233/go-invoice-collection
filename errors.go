package goinvoicecollection

import (
	"errors"
	"fmt"
)

// ErrorKind 对业务错误进行分类，调用方可用 IsKind 区分冲突、未找到等场景。
type ErrorKind string

const (
	// KindValidation 输入不合法（空号、金额为负、小数过多等）。
	KindValidation ErrorKind = "validation"
	// KindNotFound 引用的发票或收款不存在。
	KindNotFound ErrorKind = "not_found"
	// KindConflict 与已有数据冲突：幂等号内容不一致、发票号重复、撤销超额等。
	KindConflict ErrorKind = "conflict"
	// KindStorage 底层存储错误（被包装的驱动错误）。
	KindStorage ErrorKind = "storage"
)

// Error 是本包统一返回的错误类型。
type Error struct {
	Kind ErrorKind
	Msg  string
	Err  error // 可选的底层错误
}

func (e *Error) Error() string {
	if e.Err != nil {
		return string(e.Kind) + ": " + e.Msg + ": " + e.Err.Error()
	}
	return string(e.Kind) + ": " + e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// Is 支持 errors.Is(err, &Error{Kind: ...}) 这种按类别匹配，
// 同时也支持与具体消息完全相同的 *Error 相等。
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	if t.Kind != "" && t.Kind != e.Kind {
		return false
	}
	if t.Msg != "" && t.Msg != e.Msg {
		return false
	}
	return true
}

func kindError(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

func wrapError(kind ErrorKind, msg string, err error) *Error {
	return &Error{Kind: kind, Msg: msg, Err: err}
}

// IsKind 判断错误是否属于给定类别。
func IsKind(err error, kind ErrorKind) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind == kind
	}
	return false
}

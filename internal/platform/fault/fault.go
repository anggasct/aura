package fault

import "errors"

type Code interface{ ~string }

type Error[C Code] struct {
	Code   C
	Detail string
	cause  error
}

func New[C Code](code C, detail string) error {
	return &Error[C]{Code: code, Detail: detail}
}

func Wrap[C Code](code C, detail string, cause error) error {
	return &Error[C]{Code: code, Detail: detail, cause: cause}
}

func (e *Error[C]) Error() string {
	if e == nil {
		return "<nil>"
	}
	text := string(e.Code)
	if e.Detail != "" {
		text += ": " + e.Detail
	}
	if e.cause != nil {
		text += ": " + e.cause.Error()
	}
	return text
}

func (e *Error[C]) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func CodeOf[C Code](err error) (C, bool) {
	var target *Error[C]
	if errors.As(err, &target) && target != nil {
		return target.Code, true
	}
	var zero C
	return zero, false
}

func HasCode[C Code](err error, code C) bool {
	c, ok := CodeOf[C](err)
	return ok && c == code
}

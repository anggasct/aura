package command

import "errors"

type UsageError struct {
	Err error
}

func (e *UsageError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *UsageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func NewUsageError(err error) error {
	if err == nil {
		return nil
	}
	return &UsageError{Err: err}
}

func IsUsageError(err error) bool {
	var target *UsageError
	return errors.As(err, &target)
}

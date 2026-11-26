package store

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument    ErrorCode = "invalid_argument"
	ErrorCodeStorageUnavailable ErrorCode = "storage_unavailable"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

func classify(err error) error {
	return sqlite.Classify(err)
}

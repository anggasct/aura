package terminal

import "github.com/anggasct/aura/internal/platform/fault"

type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
)

func errNilArgument(name string) error {
	return fault.New(ErrorCodeInvalidArgument, name+" must not be nil")
}

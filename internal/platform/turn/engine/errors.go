package runtimeengine

import (
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
)

func codedError(code turn.ErrorCode, detail string, cause error) error {
	return fault.Wrap(code, detail, cause)
}

func invalidArgument(detail string) error {
	return fault.New(turn.ErrorCodeInvalidArgument, detail)
}

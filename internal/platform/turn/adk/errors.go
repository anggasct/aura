package runtimeadk

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/turn"
)

func codedError(code turn.ErrorCode, detail string, cause error) error {
	if cause == nil {
		return &turn.Error{Code: code, Detail: detail}
	}
	return fmt.Errorf("%w: %w", &turn.Error{Code: code, Detail: detail}, cause)
}

func invalidArgument(detail string) error {
	return &turn.Error{Code: turn.ErrorCodeInvalidArgument, Detail: detail}
}

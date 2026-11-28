package embedding

import (
	"fmt"

	"github.com/anggasct/aura/internal/platform/fault"
)

type ErrorCode string

const (
	ErrorCodeConfigInvalid     ErrorCode = "embedding_config_invalid"
	ErrorCodeRequestInvalid    ErrorCode = "embedding_request_invalid"
	ErrorCodeRequestFailed     ErrorCode = "embedding_request_failed"
	ErrorCodeStatusRejected    ErrorCode = "embedding_status_rejected"
	ErrorCodeResponseInvalid   ErrorCode = "embedding_response_invalid"
	ErrorCodeRedirectRefused   ErrorCode = "embedding_redirect_refused"
	ErrorCodeDestinationDenied ErrorCode = "embedding_destination_denied"
	ErrorCodeVectorInvalid     ErrorCode = "embedding_vector_invalid"
)

func Errorf(code ErrorCode, format string, args ...any) error {
	return fault.New(code, fmt.Sprintf(format, args...))
}

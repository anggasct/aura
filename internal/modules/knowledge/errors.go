package knowledge

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeInvalidArgument   ErrorCode = "invalid_argument"
	ErrorCodeSourceTooLarge    ErrorCode = "kb_source_too_large"
	ErrorCodeSourceUnsupported ErrorCode = "kb_source_unsupported"
	ErrorCodeSourceNotRegular  ErrorCode = "kb_source_not_regular"
	ErrorCodeSourceNotUTF8     ErrorCode = "kb_source_not_utf8"
	ErrorCodeURLRefused        ErrorCode = "kb_url_refused"
	ErrorCodeFetchFailed       ErrorCode = "kb_fetch_failed"
	ErrorCodeSecretInvalid     ErrorCode = "kb_secret_invalid"
	ErrorCodeSecretUnavailable ErrorCode = "kb_secret_unavailable"
	ErrorCodeCapExceeded       ErrorCode = "kb_cap_exceeded"
	ErrorCodeCollectionInvalid ErrorCode = "kb_collection_invalid"
	ErrorCodeSourceNotFound    ErrorCode = "kb_source_not_found"
	ErrorCodeIndexDamaged      ErrorCode = "kb_index_damaged"

	ErrorCodeEmbeddingUnavailable ErrorCode = "kb_embedding_unavailable"
	ErrorCodeEmbeddingFailed      ErrorCode = "kb_embedding_failed"
	ErrorCodeReembedRequired      ErrorCode = "kb_reembed_required"
)

type Error struct {
	Code   ErrorCode
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func CodeOf(err error) (ErrorCode, bool) {
	var target *Error
	if !errors.As(err, &target) {
		return "", false
	}
	return target.Code, true
}

func Errorf(code ErrorCode, format string, args ...any) error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

package media

type ErrorCode string

const (
	ErrorCodeInvalidArgument ErrorCode = "invalid_argument"
	ErrorCodeSizeExceeded    ErrorCode = "size_exceeded"
	ErrorCodeIncomplete      ErrorCode = "incomplete"
	ErrorCodeSpoolFailed     ErrorCode = "spool_failed"
	ErrorCodeNotText         ErrorCode = "not_text"
)

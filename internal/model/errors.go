package model

import "errors"

var (
	ErrRateLimited      = errors.New("model: rate limited")
	ErrAuthFailed       = errors.New("model: authentication failed")
	ErrModelNotFound    = errors.New("model: model not found")
	ErrOverloaded       = errors.New("model: provider overloaded")
	ErrContentFiltered  = errors.New("model: content filtered")
	ErrContextTooLong   = errors.New("model: context too long")
	ErrInvalidToolCall  = errors.New("model: invalid tool call")
	ErrConnectionFailed = errors.New("model: connection failed")
	ErrStreamIdle       = errors.New("model: stream idle timeout")
)

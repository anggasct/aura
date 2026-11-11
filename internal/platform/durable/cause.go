package durable

import "errors"

var (
	ErrRunCancelled  = errors.New("durable run cancelled")
	ErrHostStopping  = errors.New("durable host stopping")
	ErrOwnershipLost = errors.New("durable run ownership lost")
)

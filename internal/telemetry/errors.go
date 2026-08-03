package telemetry

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorCodeExporterInvalid  ErrorCode = "telemetry_exporter_invalid"
	ErrorCodePipelineFailed   ErrorCode = "telemetry_pipeline_failed"
	ErrorCodeInstrumentFailed ErrorCode = "telemetry_instrument_failed"
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

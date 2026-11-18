package telemetry

type ErrorCode string

const (
	ErrorCodeExporterInvalid  ErrorCode = "telemetry_exporter_invalid"
	ErrorCodePipelineFailed   ErrorCode = "telemetry_pipeline_failed"
	ErrorCodeInstrumentFailed ErrorCode = "telemetry_instrument_failed"
)

package telemetry

const SemconvVersion = "1.30.0"

const (
	SpanTurn  = "turn"
	SpanModel = "model"
	SpanTool  = "tool"
)

const (
	MetricTurnsTotal       = "runtime.turns.total"
	MetricTurnDuration     = "runtime.turn.duration"
	MetricModelDuration    = "gen_ai.client.operation.duration"
	MetricToolCallsTotal   = "tools.calls.total"
	MetricToolCallDuration = "tools.call.duration"
)

const (
	AttrSessionID    = "session.id"
	AttrTurnID       = "turn.id"
	AttrOrigin       = "turn.origin"
	AttrTerminalKind = "turn.terminal_kind"

	AttrGenAISystem           = "gen_ai.system"
	AttrGenAIRequestModel     = "gen_ai.request.model"
	AttrGenAIResponseModel    = "gen_ai.response.model"
	AttrGenAIOperationName    = "gen_ai.operation.name"
	AttrGenAIUsageInputCount  = "gen_ai.usage.input_tokens"
	AttrGenAIUsageOutputCount = "gen_ai.usage.output_tokens"

	AttrToolName   = "tool.name"
	AttrToolStatus = "tool.status"

	AttrToolOutputBucket = "tool.output.bucket"
	AttrToolOutputBytes  = "tool.output.bytes"

	AttrSemconvVersion = "aura.semconv.version"
)

var turnSpanAttrs = []string{
	AttrSessionID,
	AttrTurnID,
	AttrOrigin,
	AttrTerminalKind,
	AttrSemconvVersion,
}

var modelSpanAttrs = []string{
	AttrGenAISystem,
	AttrGenAIRequestModel,
	AttrGenAIResponseModel,
	AttrGenAIOperationName,
	AttrGenAIUsageInputCount,
	AttrGenAIUsageOutputCount,
}

var toolSpanAttrs = []string{
	AttrToolName,
	AttrToolStatus,
	AttrToolOutputBytes,
	AttrSemconvVersion,
}

var metricLabelAttrs = map[string][]string{
	MetricTurnsTotal:       {AttrOrigin, AttrTerminalKind},
	MetricTurnDuration:     {AttrOrigin, AttrTerminalKind},
	MetricModelDuration:    {AttrGenAISystem, AttrGenAIOperationName},
	MetricToolCallsTotal:   {AttrToolName, AttrToolStatus, AttrToolOutputBucket},
	MetricToolCallDuration: {AttrToolName, AttrToolStatus, AttrToolOutputBucket},
}

func AllowedSpanAttrs(spanName string) []string {
	switch spanName {
	case SpanTurn:
		return turnSpanAttrs
	case SpanModel:
		return modelSpanAttrs
	case SpanTool:
		return toolSpanAttrs
	default:
		return nil
	}
}

func AllowedMetricLabels(metricName string) []string {
	return metricLabelAttrs[metricName]
}

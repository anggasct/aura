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
	AttrAgentID      = "agent.id"

	AttrGenAISystem           = "gen_ai.system"
	AttrGenAIRequestModel     = "gen_ai.request.model"
	AttrGenAIResponseModel    = "gen_ai.response.model"
	AttrGenAIOperationName    = "gen_ai.operation.name"
	AttrGenAIUsageInputCount  = "gen_ai.usage.input_tokens"
	AttrGenAIUsageOutputCount = "gen_ai.usage.output_tokens"

	AttrToolName   = "tool.name"
	AttrToolStatus = "tool.status"

	AttrToolPolicyOutcome = "tool.policy.outcome"
	AttrToolApproval      = "tool.approval"
	AttrToolExecutor      = "tool.executor"

	AttrToolOutputBucket = "tool.output.bucket"
	AttrToolOutputBytes  = "tool.output.bytes"

	AttrSemconvVersion = "aura.semconv.version"

	AttrModelRoute             = "aura.model.route"
	AttrModelCandidate         = "aura.model.candidate"
	AttrModelFallbackAttempt   = "aura.model.fallback_attempt"
	AttrModelCircuitState      = "aura.model.circuit_state"
	AttrModelCircuitTransition = "aura.model.circuit_transition"
	AttrModelNormalizedResult  = "aura.model.normalized_result"
)

var turnSpanAttrs = []string{
	AttrSessionID,
	AttrTurnID,
	AttrOrigin,
	AttrTerminalKind,
	AttrAgentID,
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
	AttrToolPolicyOutcome,
	AttrToolApproval,
	AttrToolExecutor,
	AttrToolOutputBytes,
	AttrSemconvVersion,
}

var metricLabelAttrs = map[string][]string{
	MetricTurnsTotal:       {AttrOrigin, AttrTerminalKind},
	MetricTurnDuration:     {AttrOrigin, AttrTerminalKind},
	MetricModelDuration:    {AttrGenAISystem, AttrGenAIOperationName},
	MetricToolCallsTotal:   {AttrToolName, AttrToolStatus, AttrToolPolicyOutcome, AttrToolApproval, AttrToolExecutor, AttrToolOutputBucket},
	MetricToolCallDuration: {AttrToolName, AttrToolStatus, AttrToolPolicyOutcome, AttrToolApproval, AttrToolExecutor, AttrToolOutputBucket},
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

package telemetry

const SemconvVersion = "1.30.0"

const (
	SpanTurn         = "turn"
	SpanModel        = "model"
	SpanTool         = "tool"
	SpanMemoryRecall = "memory_recall"
)

const (
	MetricTurnsTotal            = "runtime.turns.total"
	MetricTurnDuration          = "runtime.turn.duration"
	MetricModelDuration         = "gen_ai.client.operation.duration"
	MetricToolCallsTotal        = "tools.calls.total"
	MetricToolCallDuration      = "tools.call.duration"
	MetricMemoryRecallsTotal    = "memory.recalls.total"
	MetricMemoryRecallDuration  = "memory.recall.duration"
	MetricMemoryRecallDocuments = "memory.recall.documents"
	MetricMCPCallsTotal         = "mcp.calls.total"
	MetricMCPCallDuration       = "mcp.call.duration"
	MetricMCPResponseSize       = "mcp.response.size"

	MetricChildSpawnsTotal  = "child.spawns.total"
	MetricChildSettlesTotal = "child.settles.total"
	MetricChildActive       = "child.active"
	MetricChildBudgetTokens = "child.budget.tokens"
	MetricChildBudgetCost   = "child.budget.cost"

	MetricProfileExtractionsTotal     = "profile.extractions.total"
	MetricProfileExtractionQueueAge   = "profile.extraction.queue_age"
	MetricProfileExtractionQueueDepth = "profile.extraction.queue_depth"
	MetricProfileFactsCount           = "profile.facts.count"
	MetricProfileExpiryLag            = "profile.expiry.lag"
	MetricProfileContextFacts         = "profile.context.facts"
	MetricProfileContextTokens        = "profile.context.tokens"
)

const (
	AttrChildState  = "child.state"
	AttrChildResult = "child.result"

	AttrSessionID       = "session.id"
	AttrTurnID          = "turn.id"
	AttrProfileResult   = "profile.result"
	AttrProfileStatus   = "profile.status"
	AttrProfileCategory = "profile.category"
	AttrOrigin          = "turn.origin"
	AttrTerminalKind    = "turn.terminal_kind"
	AttrAgentID         = "agent.id"

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

	AttrRecallOutcome        = "aura.recall.outcome"
	AttrRecallSummarized     = "aura.recall.summarized"
	AttrRecallDocuments      = "aura.recall.documents"
	AttrRecallScreened       = "aura.recall.screened"
	AttrRecallScores         = "aura.recall.scores"
	AttrRecallSources        = "aura.recall.sources"
	AttrRecallModelSystem    = "aura.recall.model_system"
	AttrRecallModelName      = "aura.recall.model_name"
	AttrRecallSummaryVersion = "aura.recall.summary_version"

	AttrMCPServer    = "mcp.server"
	AttrMCPTransport = "mcp.transport"
	AttrMCPProtocol  = "mcp.protocol"
	AttrMCPTool      = "mcp.tool"
	AttrMCPOutcome   = "mcp.outcome"
	AttrMCPCode      = "mcp.code"
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

var memoryRecallSpanAttrs = []string{
	AttrRecallOutcome,
	AttrRecallSummarized,
	AttrRecallDocuments,
	AttrRecallScreened,
	AttrRecallScores,
	AttrRecallSources,
	AttrRecallModelSystem,
	AttrRecallModelName,
	AttrRecallSummaryVersion,
	AttrSemconvVersion,
}

var metricLabelAttrs = map[string][]string{
	MetricTurnsTotal:                {AttrOrigin, AttrTerminalKind},
	MetricTurnDuration:              {AttrOrigin, AttrTerminalKind},
	MetricModelDuration:             {AttrGenAISystem, AttrGenAIOperationName},
	MetricToolCallsTotal:            {AttrToolName, AttrToolStatus, AttrToolPolicyOutcome, AttrToolApproval, AttrToolExecutor, AttrToolOutputBucket},
	MetricToolCallDuration:          {AttrToolName, AttrToolStatus, AttrToolPolicyOutcome, AttrToolApproval, AttrToolExecutor, AttrToolOutputBucket},
	MetricMemoryRecallsTotal:        {AttrRecallOutcome, AttrRecallModelSystem},
	MetricMemoryRecallDuration:      {AttrRecallOutcome},
	MetricMemoryRecallDocuments:     {AttrRecallOutcome},
	MetricMCPCallsTotal:             {AttrMCPServer, AttrMCPTransport, AttrMCPProtocol, AttrMCPTool, AttrMCPOutcome, AttrMCPCode},
	MetricMCPCallDuration:           {AttrMCPServer, AttrMCPProtocol, AttrMCPOutcome},
	MetricMCPResponseSize:           {AttrMCPServer, AttrMCPProtocol, AttrMCPOutcome},
	MetricChildSpawnsTotal:          {AttrChildResult},
	MetricChildSettlesTotal:         {AttrChildState, AttrChildResult},
	MetricChildActive:               {AttrChildState},
	MetricChildBudgetTokens:         nil,
	MetricChildBudgetCost:           nil,
	MetricProfileExtractionsTotal:   {AttrProfileResult},
	MetricProfileExtractionQueueAge: {AttrProfileResult},
	MetricProfileFactsCount:         {AttrProfileStatus, AttrProfileCategory},
}

func AllowedSpanAttrs(spanName string) []string {
	switch spanName {
	case SpanTurn:
		return turnSpanAttrs
	case SpanModel:
		return modelSpanAttrs
	case SpanTool:
		return toolSpanAttrs
	case SpanMemoryRecall:
		return memoryRecallSpanAttrs
	default:
		return nil
	}
}

func AllowedMetricLabels(metricName string) []string {
	return metricLabelAttrs[metricName]
}

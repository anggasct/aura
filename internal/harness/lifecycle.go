package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

type Phase string

const (
	PhaseAdmit        Phase = "admit"
	PhaseCanonicalize Phase = "canonicalize"
	PhasePolicy       Phase = "policy"
	PhaseApproval     Phase = "approval"
	PhasePrepare      Phase = "prepare"
	PhaseExecute      Phase = "execute"
	PhaseNormalize    Phase = "normalize"
	PhaseObserve      Phase = "observe"
	PhaseSettle       Phase = "settle"
)

var phaseOrder = [...]Phase{
	PhaseAdmit,
	PhaseCanonicalize,
	PhasePolicy,
	PhaseApproval,
	PhasePrepare,
	PhaseExecute,
	PhaseNormalize,
	PhaseObserve,
	PhaseSettle,
}

type Outcome string

const (
	OutcomeAllow           Outcome = "allow"
	OutcomeDeny            Outcome = "deny"
	OutcomeRequireApproval Outcome = "require_approval"
)

type State string

const (
	StateSucceeded State = "succeeded"
	StateDenied    State = "denied"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

type Request struct {
	SessionID      string
	TurnID         string
	InvocationID   string
	PrincipalID    string
	Capability     string
	ToolName       string
	Arguments      json.RawMessage
	IdempotencyKey string
	Deadline       time.Time
}

type Admission struct {
	Request Request
}

type CanonicalRequest struct {
	Admission Admission
	Arguments json.RawMessage
	Digest    string
}

type PolicyDecision struct {
	Outcome          Outcome
	PolicyVersion    string
	CapabilityDigest string
	RequiresApproval bool
}

type Approval struct {
	GrantID   string
	ExpiresAt time.Time
}

type PreparedInvocation struct {
	Request    CanonicalRequest
	Decision   PolicyDecision
	Approval   Approval
	Executable bool
}

type Execution struct {
	State           State
	Output          json.RawMessage
	ErrorCode       string
	ProviderReceipt json.RawMessage
}

type NormalizedResult struct {
	State     State
	Output    json.RawMessage
	ErrorCode string
}

type Observation struct {
	Result NormalizedResult
}

type Settlement struct {
	State        State
	ResultDigest string
}

type Event struct {
	ID            string
	SessionID     string
	TurnID        string
	InvocationID  string
	Kind          string
	SchemaVersion uint16
	Payload       json.RawMessage
	CreatedAt     time.Time
}

type DurableSink interface {
	Append(context.Context, *Event) error
}

type LiveSink interface {
	Publish(context.Context, *Event) error
}

type Phases struct {
	Admit        func(context.Context, *Request) (Admission, error)
	Canonicalize func(context.Context, Admission) (CanonicalRequest, error)
	Policy       func(context.Context, CanonicalRequest) (PolicyDecision, error)
	Approve      func(context.Context, CanonicalRequest, PolicyDecision) (Approval, error)
	Prepare      func(context.Context, CanonicalRequest, PolicyDecision, Approval) (PreparedInvocation, error)
	Execute      func(context.Context, PreparedInvocation) (Execution, error)
	Normalize    func(context.Context, PreparedInvocation, Execution) (NormalizedResult, error)
	Observe      func(context.Context, NormalizedResult) (Observation, error)
	Settle       func(context.Context, Observation) (Settlement, error)
}

type Config struct {
	MaxArgumentBytes int
	MaxResultBytes   int
	Now              func() time.Time
}

type Runner struct {
	cfg     Config
	phases  Phases
	durable DurableSink
	live    LiveSink
}

type Result struct {
	Settlement   Settlement
	LiveFailures int
	LiveErrors   []error
}

func NewRunner(cfg Config, phases Phases, durable DurableSink, live LiveSink) (*Runner, error) {
	if durable == nil {
		return nil, invalidArgument("durable sink must not be nil")
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := validatePhases(phases); err != nil {
		return nil, err
	}
	return &Runner{cfg: cfg, phases: phases, durable: durable, live: live}, nil
}

func (c *Config) applyDefaults() {
	if c.MaxArgumentBytes <= 0 {
		c.MaxArgumentBytes = 1 << 20
	}
	if c.MaxResultBytes <= 0 {
		c.MaxResultBytes = 4 << 20
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
}

func (c *Config) validate() error {
	var problems []error
	if c.MaxArgumentBytes <= 0 {
		problems = append(problems, invalidArgument("max argument bytes must be positive"))
	}
	if c.MaxResultBytes <= 0 {
		problems = append(problems, invalidArgument("max result bytes must be positive"))
	}
	if c.Now == nil {
		problems = append(problems, invalidArgument("clock must not be nil"))
	}
	return errors.Join(problems...)
}

func validatePhases(phases Phases) error {
	checks := []struct {
		phase      Phase
		configured bool
	}{
		{PhaseAdmit, phases.Admit != nil},
		{PhaseCanonicalize, phases.Canonicalize != nil},
		{PhasePolicy, phases.Policy != nil},
		{PhaseApproval, phases.Approve != nil},
		{PhasePrepare, phases.Prepare != nil},
		{PhaseExecute, phases.Execute != nil},
		{PhaseNormalize, phases.Normalize != nil},
		{PhaseObserve, phases.Observe != nil},
		{PhaseSettle, phases.Settle != nil},
	}
	var problems []error
	for _, check := range checks {
		if !check.configured {
			problems = append(problems, codedError(ErrorCodePhaseInvalid, string(check.phase)+" phase is not configured", nil))
		}
	}
	return errors.Join(problems...)
}

func (r *Runner) Run(ctx context.Context, req *Request) (Result, error) {
	if ctx == nil {
		return Result{}, invalidArgument("context must not be nil")
	}
	if err := validateRequest(req, r.cfg.MaxArgumentBytes); err != nil {
		return Result{}, err
	}
	expected := cloneRequest(req)
	req = &expected
	if !req.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
		defer cancel()
	}
	admitRequest := cloneRequest(req)

	admission, err := r.phases.Admit(ctx, &admitRequest)
	if err != nil {
		return Result{}, r.fail(ctx, req, PhaseAdmit, err)
	}
	if err := validateAdmission(&admission, req, r.cfg.MaxArgumentBytes); err != nil {
		return Result{}, r.fail(ctx, req, PhaseAdmit, err)
	}
	result := Result{}
	failures, emitErr := r.emit(ctx, req, PhaseAdmit, "", "")
	if applyErr := r.applyEmission(&result, failures, emitErr); applyErr != nil {
		return result, applyErr
	}

	canonical, err := r.phases.Canonicalize(ctx, admission)
	if err != nil {
		return Result{}, r.fail(ctx, req, PhaseCanonicalize, err)
	}
	if err := validateCanonical(&canonical, &admission, r.cfg.MaxArgumentBytes); err != nil {
		return Result{}, r.fail(ctx, req, PhaseCanonicalize, err)
	}
	failures, emitErr = r.emit(ctx, req, PhaseCanonicalize, "", "")
	if applyErr := r.applyEmission(&result, failures, emitErr); applyErr != nil {
		return result, applyErr
	}

	decision, err := r.phases.Policy(ctx, canonical)
	if err != nil {
		return Result{}, r.fail(ctx, req, PhasePolicy, err)
	}
	if err := validateDecision(decision); err != nil {
		return Result{}, r.fail(ctx, req, PhasePolicy, err)
	}
	failures, emitErr = r.emit(ctx, req, PhasePolicy, string(decision.Outcome), "")
	if applyErr := r.applyEmission(&result, failures, emitErr); applyErr != nil {
		return result, applyErr
	}

	approval, err := r.phases.Approve(ctx, canonical, decision)
	if err != nil {
		return Result{}, r.fail(ctx, req, PhaseApproval, err)
	}
	if err := validateApproval(approval, decision, r.cfg.Now()); err != nil {
		return Result{}, r.fail(ctx, req, PhaseApproval, err)
	}
	failures, emitErr = r.emit(ctx, req, PhaseApproval, string(decision.Outcome), "")
	if applyErr := r.applyEmission(&result, failures, emitErr); applyErr != nil {
		return result, applyErr
	}

	prepared, err := r.phases.Prepare(ctx, canonical, decision, approval)
	if err != nil {
		return Result{}, r.fail(ctx, req, PhasePrepare, err)
	}
	if err := validatePrepared(&prepared, &canonical, decision, approval); err != nil {
		return Result{}, r.fail(ctx, req, PhasePrepare, err)
	}
	failures, emitErr = r.emit(ctx, req, PhasePrepare, string(decision.Outcome), "")
	if applyErr := r.applyEmission(&result, failures, emitErr); applyErr != nil {
		return result, applyErr
	}

	var execution Execution
	switch {
	case ctx.Err() != nil:
		execution = Execution{State: StateCancelled, ErrorCode: string(ErrorCodeCancelled)}
	case !prepared.Executable:
		execution = Execution{State: StateDenied, ErrorCode: string(decision.Outcome)}
	default:
		execution, err = r.phases.Execute(ctx, prepared)
		if err != nil {
			if ctx.Err() != nil {
				execution = Execution{State: StateCancelled, ErrorCode: string(ErrorCodeCancelled)}
			} else {
				execution = Execution{State: StateFailed, ErrorCode: string(ErrorCodeProviderFailed)}
			}
		}
	}
	if ctx.Err() != nil && execution.State == StateSucceeded {
		execution = Execution{State: StateCancelled, ErrorCode: string(ErrorCodeCancelled)}
	}
	if err := validateExecution(&execution, r.cfg.MaxResultBytes); err != nil {
		return Result{}, r.fail(ctx, req, PhaseExecute, err)
	}
	failures, emitErr = r.emit(ctx, req, PhaseExecute, string(decision.Outcome), string(execution.State))
	if applyErr := r.applyEmission(&result, failures, emitErr); applyErr != nil {
		return result, applyErr
	}

	normalized, err := r.phases.Normalize(context.WithoutCancel(ctx), prepared, execution)
	if err != nil {
		return Result{}, r.fail(ctx, req, PhaseNormalize, err)
	}
	if err := validateNormalized(normalized, r.cfg.MaxResultBytes); err != nil {
		return Result{}, r.fail(ctx, req, PhaseNormalize, err)
	}
	failures, emitErr = r.emit(ctx, req, PhaseNormalize, string(decision.Outcome), string(normalized.State))
	if applyErr := r.applyEmission(&result, failures, emitErr); applyErr != nil {
		return result, applyErr
	}

	observation, err := r.phases.Observe(context.WithoutCancel(ctx), normalized)
	if err != nil {
		return Result{}, r.fail(ctx, req, PhaseObserve, err)
	}
	if err := validateObservation(&observation, &normalized); err != nil {
		return Result{}, r.fail(ctx, req, PhaseObserve, err)
	}
	failures, emitErr = r.emit(ctx, req, PhaseObserve, string(decision.Outcome), string(normalized.State))
	if applyErr := r.applyEmission(&result, failures, emitErr); applyErr != nil {
		return result, applyErr
	}

	settlement, err := r.phases.Settle(context.WithoutCancel(ctx), observation)
	if err != nil {
		return Result{}, r.fail(ctx, req, PhaseSettle, err)
	}
	if err := validateSettlement(&settlement, &normalized); err != nil {
		return Result{}, r.fail(ctx, req, PhaseSettle, err)
	}
	failures, emitErr = r.emit(ctx, req, PhaseSettle, string(decision.Outcome), string(settlement.State))
	if applyErr := r.applyEmission(&result, failures, emitErr); applyErr != nil {
		return result, applyErr
	}
	result.Settlement = settlement
	return result, nil
}

func (r *Runner) fail(ctx context.Context, req *Request, phase Phase, cause error) error {
	if req == nil {
		return cause
	}
	code := ErrorCodeLifecycleInvalid
	if coded, ok := CodeOf(cause); ok {
		code = coded
	}
	_, emitErr := r.emit(context.WithoutCancel(ctx), req, phase, "", "", string(code))
	if emitErr != nil {
		if liveCode, ok := CodeOf(emitErr); ok && liveCode == ErrorCodeLivePublishFailed {
			return cause
		}
		return errors.Join(cause, emitErr)
	}
	return cause
}

func (r *Runner) emit(ctx context.Context, req *Request, phase Phase, outcome, state string, errorCodes ...string) (int, error) {
	errorCode := ""
	if len(errorCodes) > 0 {
		errorCode = errorCodes[0]
	}
	payload, err := json.Marshal(struct {
		Phase     Phase  `json:"phase"`
		Outcome   string `json:"outcome,omitempty"`
		State     string `json:"state,omitempty"`
		ErrorCode string `json:"error_code,omitempty"`
	}{Phase: phase, Outcome: outcome, State: state, ErrorCode: errorCode})
	if err != nil {
		return 0, codedError(ErrorCodeLifecycleInvalid, "marshal lifecycle event", err)
	}
	event := &Event{
		ID:            req.InvocationID + "." + string(phase),
		SessionID:     req.SessionID,
		TurnID:        req.TurnID,
		InvocationID:  req.InvocationID,
		Kind:          "invocation." + string(phase),
		SchemaVersion: 1,
		Payload:       payload,
		CreatedAt:     r.cfg.Now().UTC(),
	}
	if err := r.durable.Append(context.WithoutCancel(ctx), event); err != nil {
		return 0, codedError(ErrorCodeDurabilityFailed, "append lifecycle event", err)
	}
	if r.live == nil {
		return 0, nil
	}
	if err := r.live.Publish(ctx, event); err != nil {
		return 1, codedError(ErrorCodeLivePublishFailed, "publish lifecycle event", err)
	}
	return 0, nil
}

func (r *Runner) applyEmission(result *Result, failures int, err error) error {
	result.LiveFailures += failures
	if err == nil {
		return nil
	}
	if code, ok := CodeOf(err); ok && code == ErrorCodeLivePublishFailed {
		result.LiveErrors = append(result.LiveErrors, err)
		return nil
	}
	return err
}

func validateRequest(req *Request, maxBytes int) error {
	if req == nil {
		return invalidArgument("request must not be nil")
	}
	if req.SessionID == "" || req.TurnID == "" || req.InvocationID == "" || req.PrincipalID == "" {
		return invalidArgument("request identity fields must not be empty")
	}
	if req.Capability == "" || req.ToolName == "" || req.IdempotencyKey == "" {
		return invalidArgument("capability, tool name, and idempotency key must not be empty")
	}
	if len(req.Arguments) == 0 || len(req.Arguments) > maxBytes || !json.Valid(req.Arguments) {
		return invalidArgument("request arguments must be bounded valid JSON")
	}
	return nil
}

func validateAdmission(admission *Admission, req *Request, maxBytes int) error {
	if admission == nil {
		return invalidArgument("admission must not be nil")
	}
	if !sameRequest(&admission.Request, req) {
		return codedError(ErrorCodeLifecycleInvalid, "admission changed request identity", nil)
	}
	return validateRequest(&admission.Request, maxBytes)
}

func validateCanonical(canonical *CanonicalRequest, admission *Admission, maxBytes int) error {
	if canonical == nil || admission == nil {
		return invalidArgument("canonical request and admission must not be nil")
	}
	if !sameRequest(&canonical.Admission.Request, &admission.Request) {
		return codedError(ErrorCodeLifecycleInvalid, "canonical request changed invocation identity", nil)
	}
	if len(canonical.Arguments) == 0 || len(canonical.Arguments) > maxBytes || !json.Valid(canonical.Arguments) {
		return invalidArgument("canonical arguments must be bounded valid JSON")
	}
	sum := sha256.Sum256(canonical.Arguments)
	if canonical.Digest != hex.EncodeToString(sum[:]) {
		return codedError(ErrorCodeLifecycleInvalid, "canonical request digest does not match arguments", nil)
	}
	return nil
}

func validateDecision(decision PolicyDecision) error {
	switch decision.Outcome {
	case OutcomeAllow, OutcomeDeny, OutcomeRequireApproval:
	default:
		return codedError(ErrorCodeLifecycleInvalid, "unknown policy outcome", nil)
	}
	if decision.PolicyVersion == "" || decision.CapabilityDigest == "" {
		return codedError(ErrorCodeLifecycleInvalid, "policy decision is missing version or capability digest", nil)
	}
	if decision.Outcome == OutcomeRequireApproval && !decision.RequiresApproval {
		return codedError(ErrorCodeLifecycleInvalid, "approval outcome must require approval", nil)
	}
	if decision.Outcome == OutcomeDeny && decision.RequiresApproval {
		return codedError(ErrorCodeLifecycleInvalid, "denied outcome cannot require approval", nil)
	}
	return nil
}

func validateApproval(approval Approval, decision PolicyDecision, now time.Time) error {
	if decision.RequiresApproval && approval.GrantID == "" {
		return codedError(ErrorCodeLifecycleInvalid, "approved invocation is missing a grant", nil)
	}
	if !decision.RequiresApproval && approval.GrantID != "" {
		return codedError(ErrorCodeLifecycleInvalid, "denied invocation cannot carry a grant", nil)
	}
	if decision.RequiresApproval && !approval.ExpiresAt.After(now) {
		return codedError(ErrorCodeLifecycleInvalid, "approval grant is expired", nil)
	}
	return nil
}

func validatePrepared(prepared *PreparedInvocation, canonical *CanonicalRequest, decision PolicyDecision, approval Approval) error {
	if prepared == nil || canonical == nil {
		return invalidArgument("prepared invocation and canonical request must not be nil")
	}
	if !sameCanonical(&prepared.Request, canonical) || prepared.Decision != decision || prepared.Approval != approval {
		return codedError(ErrorCodeLifecycleInvalid, "prepared invocation changed a bound value", nil)
	}
	wantExecutable := (decision.Outcome == OutcomeAllow || decision.Outcome == OutcomeRequireApproval) && (!decision.RequiresApproval || approval.GrantID != "")
	if prepared.Executable != wantExecutable {
		return codedError(ErrorCodeLifecycleInvalid, "prepared invocation has an invalid execution gate", nil)
	}
	return nil
}

func validateExecution(execution *Execution, maxBytes int) error {
	if execution == nil {
		return invalidArgument("execution must not be nil")
	}
	if !validState(execution.State) {
		return codedError(ErrorCodeLifecycleInvalid, "execution returned an unknown state", nil)
	}
	if len(execution.Output) > maxBytes || (len(execution.Output) > 0 && !json.Valid(execution.Output)) {
		return invalidArgument("execution output must be bounded valid JSON")
	}
	if len(execution.ProviderReceipt) > maxBytes || (len(execution.ProviderReceipt) > 0 && !json.Valid(execution.ProviderReceipt)) {
		return invalidArgument("provider receipt must be bounded valid JSON")
	}
	return nil
}

func validateNormalized(result NormalizedResult, maxBytes int) error {
	if !validState(result.State) {
		return codedError(ErrorCodeLifecycleInvalid, "normalizer returned an unknown state", nil)
	}
	if len(result.Output) > maxBytes || (len(result.Output) > 0 && !json.Valid(result.Output)) {
		return invalidArgument("normalized output must be bounded valid JSON")
	}
	return nil
}

func validateObservation(observation *Observation, normalized *NormalizedResult) error {
	if observation == nil || normalized == nil {
		return invalidArgument("observation and normalized result must not be nil")
	}
	if observation.Result.State != normalized.State || observation.Result.ErrorCode != normalized.ErrorCode || !bytes.Equal(observation.Result.Output, normalized.Output) {
		return codedError(ErrorCodeLifecycleInvalid, "observer changed the result state", nil)
	}
	return nil
}

func validateSettlement(settlement *Settlement, normalized *NormalizedResult) error {
	if settlement == nil || normalized == nil {
		return invalidArgument("settlement and normalized result must not be nil")
	}
	if !validState(settlement.State) || settlement.State != normalized.State {
		return codedError(ErrorCodeLifecycleInvalid, "settlement state does not match normalized state", nil)
	}
	expectedDigest, err := resultDigest(normalized)
	if err != nil {
		return codedError(ErrorCodeLifecycleInvalid, "digest normalized result", err)
	}
	if settlement.ResultDigest != expectedDigest {
		return codedError(ErrorCodeLifecycleInvalid, "settlement result digest does not match normalized result", nil)
	}
	return nil
}

func validState(state State) bool {
	switch state {
	case StateSucceeded, StateDenied, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

func cloneRequest(req *Request) Request {
	copyReq := *req
	copyReq.Arguments = bytes.Clone(req.Arguments)
	return copyReq
}

func sameRequest(left, right *Request) bool {
	return left != nil && right != nil && left.SessionID == right.SessionID &&
		left.TurnID == right.TurnID &&
		left.InvocationID == right.InvocationID &&
		left.PrincipalID == right.PrincipalID &&
		left.Capability == right.Capability &&
		left.ToolName == right.ToolName &&
		left.IdempotencyKey == right.IdempotencyKey &&
		left.Deadline.Equal(right.Deadline) &&
		bytes.Equal(left.Arguments, right.Arguments)
}

func sameCanonical(left, right *CanonicalRequest) bool {
	return left != nil && right != nil && left.Digest == right.Digest &&
		sameRequest(&left.Admission.Request, &right.Admission.Request) &&
		bytes.Equal(left.Arguments, right.Arguments)
}

func resultDigest(result *NormalizedResult) (string, error) {
	if result == nil {
		return "", invalidArgument("normalized result must not be nil")
	}
	payload, err := json.Marshal(struct {
		State     State           `json:"state"`
		Output    json.RawMessage `json:"output,omitempty"`
		ErrorCode string          `json:"error_code,omitempty"`
	}{State: result.State, Output: result.Output, ErrorCode: result.ErrorCode})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

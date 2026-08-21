package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

type recordingDurable struct {
	events  []Event
	order   *[]string
	failAt  int
	appends int
}

func (s *recordingDurable) Append(_ context.Context, event *Event) error {
	if s.failAt >= 0 && s.appends == s.failAt {
		return errors.New("durable store unavailable")
	}
	s.appends++
	s.events = append(s.events, *event)
	if s.order != nil {
		*s.order = append(*s.order, "durable:"+event.Kind)
	}
	return nil
}

type recordingLive struct {
	events []Event
	order  *[]string
	fail   bool
}

func (s *recordingLive) Publish(_ context.Context, event *Event) error {
	if s.order != nil {
		*s.order = append(*s.order, "live:"+event.Kind)
	}
	s.events = append(s.events, *event)
	if s.fail {
		return errors.New("live sink unavailable")
	}
	return nil
}

func testRequest() *Request {
	return &Request{
		SessionID:      "session-1",
		TurnID:         "turn-1",
		InvocationID:   "invocation-1",
		PrincipalID:    "owner-1",
		Capability:     "tool.read",
		ToolName:       "read",
		Arguments:      json.RawMessage(`{"path":"main.go"}`),
		IdempotencyKey: "request-1",
	}
}

func phasesFor(calls *[]Phase, execute func(context.Context, PreparedInvocation) (Execution, error)) Phases {
	return Phases{
		Admit: func(_ context.Context, req *Request) (Admission, error) {
			*calls = append(*calls, PhaseAdmit)
			return Admission{Request: *req}, nil
		},
		Canonicalize: func(_ context.Context, admission Admission) (CanonicalRequest, error) {
			*calls = append(*calls, PhaseCanonicalize)
			sum := sha256.Sum256(admission.Request.Arguments)
			return CanonicalRequest{
				Admission: admission,
				Arguments: admission.Request.Arguments,
				Digest:    hex.EncodeToString(sum[:]),
			}, nil
		},
		Policy: func(_ context.Context, request CanonicalRequest) (PolicyDecision, error) {
			*calls = append(*calls, PhasePolicy)
			return PolicyDecision{
				Outcome:          OutcomeAllow,
				PolicyVersion:    "policy-1",
				CapabilityDigest: "capability-1",
			}, nil
		},
		Approve: func(_ context.Context, _ CanonicalRequest, _ PolicyDecision) (Approval, error) {
			*calls = append(*calls, PhaseApproval)
			return Approval{}, nil
		},
		Prepare: func(_ context.Context, request CanonicalRequest, decision PolicyDecision, approval Approval) (PreparedInvocation, error) {
			*calls = append(*calls, PhasePrepare)
			return PreparedInvocation{Request: request, Decision: decision, Approval: approval, Executable: true}, nil
		},
		Execute: func(ctx context.Context, prepared PreparedInvocation) (Execution, error) {
			*calls = append(*calls, PhaseExecute)
			return execute(ctx, prepared)
		},
		Normalize: func(_ context.Context, _ PreparedInvocation, execution Execution) (NormalizedResult, error) {
			*calls = append(*calls, PhaseNormalize)
			return NormalizedResult{State: execution.State, Output: execution.Output, ErrorCode: execution.ErrorCode}, nil
		},
		Observe: func(_ context.Context, result NormalizedResult) (Observation, error) {
			*calls = append(*calls, PhaseObserve)
			return Observation{Result: result}, nil
		},
		Settle: func(_ context.Context, observation Observation) (Settlement, error) {
			*calls = append(*calls, PhaseSettle)
			digest, err := resultDigest(&observation.Result)
			if err != nil {
				return Settlement{}, err
			}
			return Settlement{State: observation.Result.State, ResultDigest: digest}, nil
		},
	}
}

func newTestRunner(t *testing.T, phases Phases, durable DurableSink, live LiveSink) *Runner {
	t.Helper()
	runner, err := NewRunner(Config{Now: func() time.Time { return time.Unix(100, 0).UTC() }}, phases, durable, live)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return runner
}

func TestRunnerPreservesPhaseOrderAndDurableBeforeLive(t *testing.T) {
	var calls []Phase
	var order []string
	durable := &recordingDurable{order: &order, failAt: -1}
	live := &recordingLive{order: &order}
	runner := newTestRunner(t, phasesFor(&calls, func(context.Context, PreparedInvocation) (Execution, error) {
		return Execution{State: StateSucceeded, Output: json.RawMessage(`{"ok":true}`)}, nil
	}), durable, live)

	result, err := runner.Run(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Settlement.State != StateSucceeded {
		t.Fatalf("settlement state = %q, want succeeded", result.Settlement.State)
	}
	if got := len(durable.events); got != len(phaseOrder) {
		t.Fatalf("durable events = %d, want %d", got, len(phaseOrder))
	}
	if got := len(live.events); got != len(phaseOrder) {
		t.Fatalf("live events = %d, want %d", got, len(phaseOrder))
	}
	if !reflect.DeepEqual(calls, phaseOrder[:]) {
		t.Fatalf("phases = %v, want %v", calls, phaseOrder)
	}
	for i := range phaseOrder {
		if order[2*i] != "durable:invocation."+string(phaseOrder[i]) {
			t.Fatalf("order[%d] = %q, want durable event", 2*i, order[2*i])
		}
		if order[2*i+1] != "live:invocation."+string(phaseOrder[i]) {
			t.Fatalf("order[%d] = %q, want live event", 2*i+1, order[2*i+1])
		}
	}
}

func TestRunnerDenialCannotReachProvider(t *testing.T) {
	var calls []Phase
	phases := phasesFor(&calls, func(context.Context, PreparedInvocation) (Execution, error) {
		t.Fatal("provider executed for denied request")
		return Execution{}, nil
	})
	phases.Policy = func(_ context.Context, _ CanonicalRequest) (PolicyDecision, error) {
		calls = append(calls, PhasePolicy)
		return PolicyDecision{Outcome: OutcomeDeny, PolicyVersion: "policy-1", CapabilityDigest: "capability-1"}, nil
	}
	phases.Prepare = func(_ context.Context, request CanonicalRequest, decision PolicyDecision, approval Approval) (PreparedInvocation, error) {
		calls = append(calls, PhasePrepare)
		return PreparedInvocation{Request: request, Decision: decision, Approval: approval, Executable: false}, nil
	}
	durable := &recordingDurable{failAt: -1}
	runner := newTestRunner(t, phases, durable, nil)

	result, err := runner.Run(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Settlement.State != StateDenied {
		t.Fatalf("settlement state = %q, want denied", result.Settlement.State)
	}
	want := []Phase{PhaseAdmit, PhaseCanonicalize, PhasePolicy, PhaseApproval, PhasePrepare, PhaseNormalize, PhaseObserve, PhaseSettle}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("phases = %v, want %v", calls, want)
	}
}

func TestRunnerApprovalGrantOpensExecutionGate(t *testing.T) {
	var calls []Phase
	executed := false
	phases := phasesFor(&calls, func(context.Context, PreparedInvocation) (Execution, error) {
		executed = true
		return Execution{State: StateSucceeded}, nil
	})
	phases.Policy = func(_ context.Context, _ CanonicalRequest) (PolicyDecision, error) {
		calls = append(calls, PhasePolicy)
		return PolicyDecision{
			Outcome:          OutcomeRequireApproval,
			PolicyVersion:    "policy-1",
			CapabilityDigest: "capability-1",
			RequiresApproval: true,
		}, nil
	}
	phases.Approve = func(_ context.Context, _ CanonicalRequest, _ PolicyDecision) (Approval, error) {
		calls = append(calls, PhaseApproval)
		return Approval{GrantID: "grant-1", ExpiresAt: time.Unix(101, 0).UTC()}, nil
	}
	durable := &recordingDurable{failAt: -1}
	runner := newTestRunner(t, phases, durable, nil)

	result, err := runner.Run(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !executed || result.Settlement.State != StateSucceeded {
		t.Fatalf("executed = %v, settlement = %q; want successful execution", executed, result.Settlement.State)
	}
}

func TestRunnerDurabilityFailurePreventsLivePublication(t *testing.T) {
	var order []string
	durable := &recordingDurable{order: &order}
	live := &recordingLive{order: &order}
	runner := newTestRunner(t, phasesFor(&[]Phase{}, func(context.Context, PreparedInvocation) (Execution, error) {
		return Execution{State: StateSucceeded}, nil
	}), durable, live)

	_, err := runner.Run(context.Background(), testRequest())
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeDurabilityFailed {
		t.Fatalf("CodeOf(%v) = %q, %v; want durability_failed", err, code, ok)
	}
	if len(live.events) != 0 {
		t.Fatalf("live events = %d, want none", len(live.events))
	}
}

func TestRunnerLiveFailureDoesNotEraseDurableResult(t *testing.T) {
	var order []string
	durable := &recordingDurable{order: &order, failAt: -1}
	live := &recordingLive{order: &order, fail: true}
	runner := newTestRunner(t, phasesFor(&[]Phase{}, func(context.Context, PreparedInvocation) (Execution, error) {
		return Execution{State: StateSucceeded}, nil
	}), durable, live)

	result, err := runner.Run(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.LiveFailures != len(phaseOrder) {
		t.Fatalf("live failures = %d, want %d", result.LiveFailures, len(phaseOrder))
	}
	if len(durable.events) != len(phaseOrder) {
		t.Fatalf("durable events = %d, want %d", len(durable.events), len(phaseOrder))
	}
}

func TestRunnerCancellationSkipsProviderAndSettles(t *testing.T) {
	var calls []Phase
	ctx, cancel := context.WithCancel(context.Background())
	phases := phasesFor(&calls, func(context.Context, PreparedInvocation) (Execution, error) {
		t.Fatal("provider executed after cancellation")
		return Execution{}, nil
	})
	phases.Prepare = func(_ context.Context, request CanonicalRequest, decision PolicyDecision, approval Approval) (PreparedInvocation, error) {
		calls = append(calls, PhasePrepare)
		cancel()
		return PreparedInvocation{Request: request, Decision: decision, Approval: approval, Executable: true}, nil
	}
	runner := newTestRunner(t, phases, &recordingDurable{failAt: -1}, nil)

	result, err := runner.Run(ctx, testRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Settlement.State != StateCancelled {
		t.Fatalf("settlement state = %q, want cancelled", result.Settlement.State)
	}
	if !reflect.DeepEqual(calls, []Phase{PhaseAdmit, PhaseCanonicalize, PhasePolicy, PhaseApproval, PhasePrepare, PhaseNormalize, PhaseObserve, PhaseSettle}) {
		t.Fatalf("phases = %v, provider phase should be skipped", calls)
	}
}

func TestNewRunnerRequiresEveryPhase(t *testing.T) {
	_, err := NewRunner(Config{}, Phases{}, &recordingDurable{failAt: -1}, nil)
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodePhaseInvalid {
		t.Fatalf("CodeOf(%v) = %q, %v; want phase_invalid", err, code, ok)
	}
}

func TestRunnerRejectsChangedCanonicalDigest(t *testing.T) {
	var calls []Phase
	phases := phasesFor(&calls, func(context.Context, PreparedInvocation) (Execution, error) {
		return Execution{State: StateSucceeded}, nil
	})
	phases.Canonicalize = func(_ context.Context, admission Admission) (CanonicalRequest, error) {
		calls = append(calls, PhaseCanonicalize)
		return CanonicalRequest{Admission: admission, Arguments: admission.Request.Arguments, Digest: "wrong"}, nil
	}
	runner := newTestRunner(t, phases, &recordingDurable{failAt: -1}, nil)
	_, err := runner.Run(context.Background(), testRequest())
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeLifecycleInvalid {
		t.Fatalf("CodeOf(%v) = %q, %v; want lifecycle_invalid", err, code, ok)
	}
}

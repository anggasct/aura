package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

type deciderAnswer func(ctx context.Context, prompt *ApprovalPrompt, asked int) (bool, error)

func approveEvery(context.Context, *ApprovalPrompt, int) (bool, error) { return true, nil }

type scriptedDecider struct {
	answer deciderAnswer

	mu      sync.Mutex
	prompts []ApprovalPrompt
}

func (d *scriptedDecider) decide(ctx context.Context, prompt *ApprovalPrompt) (bool, error) {
	d.mu.Lock()
	d.prompts = append(d.prompts, *prompt)
	asked := len(d.prompts)
	d.mu.Unlock()
	return d.answer(ctx, prompt, asked)
}

func (d *scriptedDecider) asked() []ApprovalPrompt {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.prompts)
}

type observedApprovals struct {
	mu     sync.Mutex
	states []string
}

func (o *observedApprovals) observe(_ context.Context, observation Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.states = append(o.states, observation.Approval)
}

func (o *observedApprovals) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.states)
}

type rigOptions struct {
	requiresApproval bool
	sink             bool
	withoutDecider   bool
	access           AccessPolicy
	reachable        func(sessionID string) bool
	respond          Adapter
	answer           deciderAnswer
}

type authorizationRig struct {
	broker   *Broker
	decider  *scriptedDecider
	observed *observedApprovals
	probe    *probeTool
}

func newAuthorizationRig(t *testing.T, options *rigOptions) *authorizationRig {
	t.Helper()
	rule := &approval.Rule{ToolName: "probe", ToolVersion: "v1", RequiresApproval: options.requiresApproval}
	rig := &authorizationRig{
		decider:  &scriptedDecider{answer: options.answer},
		observed: &observedApprovals{},
		probe:    &probeTool{rule: rule, respond: options.respond},
	}
	if rig.decider.answer == nil {
		rig.decider.answer = approveEvery
	}
	brokerOptions := &Options{HostCapabilities: capabilitytest.Host(t), Observer: rig.observed.observe, Access: options.access, ApprovalReachable: options.reachable}
	if !options.withoutDecider {
		brokerOptions.ApprovalDecider = rig.decider.decide
	}
	broker, err := New(brokerOptions)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	definition := &tools.Definition{Name: "probe", Version: "v1", ExternalSink: options.sink, Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil }}
	if err := broker.RegisterTool(definition, rig.probe.adapter, rule); err != nil {
		t.Fatalf("register probe: %v", err)
	}
	rig.broker = broker
	return rig
}

func (r *authorizationRig) requireUntouched(t *testing.T) {
	t.Helper()
	if prompts := r.decider.asked(); len(prompts) != 0 {
		t.Errorf("decider calls = %d, want none", len(prompts))
	}
	if calls := r.probe.calls.Load(); calls != 0 {
		t.Errorf("tool runs = %d, want none", calls)
	}
}

func standingAccess() *fakeAccess {
	return &fakeAccess{ceilings: map[principal.Class][]string{principal.ClassTrusted: {"probe"}}, standing: true}
}

func trustedProbeRequest() *ToolRequest {
	return classifiedRequest("probe", `{"query":"x"}`, principal.ClassTrusted)
}

func requestWithin(window time.Duration) *ToolRequest {
	request := probeRequest("probe")
	request.Deadline = time.Now().Add(window)
	return request
}

var probeSucceeded = callOutcome{class: ResultOK, output: `{"ok":true}`}

func requireStates(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("observed approval states = %q, want %q", got, want)
	}
}

func authorizationKey(inv durable.Invocation) string {
	scope := durable.NewTurnScope(inv)
	scope.NextOp()
	return scope.NextOp()
}

func TestDurableApprovalAsksTheDeciderOnce(t *testing.T) {
	cases := []struct {
		name    string
		options rigOptions
		request func() *ToolRequest
		state   string
		prompts int
	}{
		{name: "approved by the decider", options: rigOptions{requiresApproval: true}, request: func() *ToolRequest { return requestWithin(time.Minute) }, state: ApprovalApproved, prompts: 1},
		{name: "auto-allowed", request: func() *ToolRequest { return requestWithin(time.Minute) }, state: ApprovalAuto},
		{name: "standing grant", options: rigOptions{requiresApproval: true, access: standingAccess()}, request: trustedProbeRequest, state: ApprovalStanding},
	}
	hosted := startHostedRuntime(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAuthorizationRig(t, &tc.options)
			request := tc.request()
			var first, replay callOutcome
			hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				first = outcomeOf(rig.broker.Execute(inTurn(ctx, inv), request))
				replay = outcomeOf(rig.broker.Execute(inTurn(ctx, inv), request))
				return nil
			})
			if first != probeSucceeded || replay != probeSucceeded {
				t.Errorf("first = %+v, replay = %+v, want %+v for both", first, replay, probeSucceeded)
			}
			if calls := rig.probe.calls.Load(); calls != 1 {
				t.Errorf("tool runs = %d, want 1", calls)
			}
			requireStates(t, rig.observed.list(), tc.state, tc.state)
			prompts := rig.decider.asked()
			if len(prompts) != tc.prompts {
				t.Fatalf("decider calls = %d, want %d", len(prompts), tc.prompts)
			}
			if tc.prompts == 0 {
				return
			}
			inMemory := newAuthorizationRig(t, &tc.options)
			if got := outcomeOf(inMemory.broker.Execute(t.Context(), request)); got != probeSucceeded {
				t.Fatalf("in-memory call = %+v, want %+v", got, probeSucceeded)
			}
			if want := inMemory.decider.asked(); len(want) != 1 || prompts[0] != want[0] {
				t.Errorf("durable prompt = %+v, want the in-memory prompt %+v", prompts[0], want)
			}
		})
	}
}

func TestDurableApprovalRefusalIsRecorded(t *testing.T) {
	cases := []struct {
		name    string
		options rigOptions
		window  time.Duration
		want    callOutcome
		state   string
		prompts int
	}{
		{
			name:    "the decider refuses",
			options: rigOptions{requiresApproval: true, answer: func(context.Context, *ApprovalPrompt, int) (bool, error) { return false, nil }},
			window:  time.Minute,
			want:    callOutcome{class: ResultApprovalRequired, text: `approval_required: tool "probe@v1" approval was rejected`},
			state:   ApprovalRejected,
			prompts: 1,
		},
		{
			name: "the decider fails",
			options: rigOptions{requiresApproval: true, answer: func(context.Context, *ApprovalPrompt, int) (bool, error) {
				return false, errors.New("card service down")
			}},
			window:  time.Minute,
			want:    callOutcome{class: ResultPolicyDenied, text: `policy_denied: tool "probe@v1" approval failed closed: card service down`},
			state:   ApprovalRejected,
			prompts: 1,
		},
		{
			name:    "no decider",
			options: rigOptions{requiresApproval: true, withoutDecider: true},
			window:  time.Minute,
			want:    callOutcome{class: ResultApprovalRequired, text: `approval_required: tool "probe@v1" requires approval`},
			state:   ApprovalMissing,
		},
		{
			name: "the decider answers after the deadline of the call",
			options: rigOptions{requiresApproval: true, answer: func(_ context.Context, prompt *ApprovalPrompt, _ int) (bool, error) {
				waitPast(prompt.ExpiresAt)
				return true, nil
			}},
			window:  hostedRequestWindow,
			want:    callOutcome{class: ResultDeadlineExceeded, text: "deadline_exceeded: tool request deadline exceeded"},
			state:   ApprovalRejected,
			prompts: 1,
		},
	}
	hosted := startHostedRuntime(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAuthorizationRig(t, &tc.options)
			again := newAuthorizationRig(t, &rigOptions{requiresApproval: true})
			request := requestWithin(tc.window)
			var first, replay callOutcome
			var took time.Duration
			hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				started := time.Now()
				first = outcomeOf(rig.broker.Execute(inTurn(ctx, inv), request))
				took = time.Since(started)
				replay = outcomeOf(again.broker.Execute(inTurn(ctx, inv), requestWithin(time.Minute)))
				return nil
			})
			if first != tc.want {
				t.Errorf("first = %+v, want %+v", first, tc.want)
			}
			if replay != tc.want {
				t.Errorf("replay with a decider that approves = %+v, want %+v", replay, tc.want)
			}
			if tc.window == time.Minute && took >= hostedWait {
				t.Errorf("the refusal took %v, want it at once", took)
			}
			if prompts := rig.decider.asked(); len(prompts) != tc.prompts {
				t.Errorf("decider calls = %d, want %d", len(prompts), tc.prompts)
			}
			if calls := rig.probe.calls.Load(); calls != 0 {
				t.Errorf("tool runs = %d, want none", calls)
			}
			again.requireUntouched(t)
			requireStates(t, rig.observed.list(), tc.state)
			requireStates(t, again.observed.list(), tc.state)
		})
	}
}

type interruption struct {
	name      string
	interrupt func(t *testing.T, hosted *hostedRuntime)
}

func TestDurableApprovalInterruptedIsAskedAgain(t *testing.T) {
	cases := []interruption{
		{name: "a lost lease", interrupt: func(t *testing.T, hosted *hostedRuntime) {
			if _, err := hosted.db.ExecContext(t.Context(), `DELETE FROM durable_host`); err != nil {
				t.Fatalf("delete the host lease: %v", err)
			}
		}},
		{name: "a host stop", interrupt: func(t *testing.T, hosted *hostedRuntime) {
			hosted.restartHost(t)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hosted := startHostedRuntime(t)
			waiting := make(chan struct{})
			rig := newAuthorizationRig(t, &rigOptions{requiresApproval: true, answer: waitThenApprove(waiting)})
			var outcomes []callOutcome
			key := hosted.start(t, func(ctx context.Context, inv durable.Invocation) error {
				outcomes = append(outcomes, outcomeOf(rig.broker.Execute(inTurn(ctx, inv), probeRequest("probe"))))
				return ctx.Err()
			})
			awaitClosed(t, waiting)
			tc.interrupt(t, hosted)
			hosted.awaitEnded(t, key, 2)

			if len(outcomes) != 2 || outcomes[0].class == ResultOK || outcomes[1] != probeSucceeded {
				t.Fatalf("outcomes = %+v, want an interrupted call and then %+v", outcomes, probeSucceeded)
			}
			if prompts := rig.decider.asked(); len(prompts) != 2 {
				t.Errorf("decider calls = %d, want 2", len(prompts))
			}
			if calls := rig.probe.calls.Load(); calls != 1 {
				t.Errorf("tool runs = %d, want 1", calls)
			}
			rows := hosted.actionRows(t, key)
			if len(rows) != 3 {
				t.Fatalf("recorded actions = %+v, want policy, authorization and execution", rows)
			}
			if record, ok := decodeAuthorization(rows[1].result); !ok || record.State != ApprovalApproved || record.Failure != nil {
				t.Errorf("authorization record = %s, want an approval", rows[1].result)
			}
		})
	}
	t.Run("a cancelled run", func(t *testing.T) {
		hosted := startHostedRuntime(t)
		waiting := make(chan struct{})
		rig := newAuthorizationRig(t, &rigOptions{requiresApproval: true, answer: waitThenApprove(waiting)})
		key := hosted.start(t, func(ctx context.Context, inv durable.Invocation) error {
			_, err := rig.broker.Execute(inTurn(ctx, inv), probeRequest("probe"))
			return err
		})
		awaitClosed(t, waiting)
		if err := hosted.rt.Cancel(t.Context(), durable.RunRef{Key: key}); err != nil {
			t.Fatalf("cancel the run: %v", err)
		}
		awaitCancelled(t, hosted, key)
		if calls := rig.probe.calls.Load(); calls != 0 {
			t.Errorf("tool runs = %d, want none", calls)
		}
		if rows := hosted.actionRows(t, key); len(rows) != 1 {
			t.Errorf("recorded actions = %+v, want the policy evaluation alone", rows)
		}
	})
}

func waitThenApprove(waiting chan struct{}) deciderAnswer {
	return func(ctx context.Context, _ *ApprovalPrompt, asked int) (bool, error) {
		if asked > 1 {
			return true, nil
		}
		close(waiting)
		<-ctx.Done()
		return false, ctx.Err()
	}
}

func awaitClosed(t *testing.T, waiting <-chan struct{}) {
	t.Helper()
	select {
	case <-waiting:
	case <-time.After(hostedWait):
		t.Fatal("the decider was never asked")
	}
}

func awaitCancelled(t *testing.T, hosted *hostedRuntime, key string) {
	t.Helper()
	deadline := time.Now().Add(hostedWait)
	for {
		status, err := hosted.rt.Status(t.Context(), durable.RunRef{Key: key})
		if err != nil {
			t.Fatalf("status of %s: %v", key, err)
		}
		if status.State != durable.RunRunning && hosted.returnedCount(key) == 1 {
			if status.State != durable.RunCancelled {
				t.Fatalf("run %s ended as %s, want cancelled", key, status.State)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s was cancelled", key)
		}
		<-time.After(2 * time.Millisecond)
	}
}

func TestDurableAuthorizationSurvivesANewProcess(t *testing.T) {
	cases := []struct {
		name    string
		options rigOptions
		prompts int
	}{
		{name: "auto-allowed"},
		{name: "approved by the decider", options: rigOptions{requiresApproval: true}, prompts: 1},
	}
	hosted := startHostedRuntime(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			running := make(chan struct{})
			before := tc.options
			before.respond = func(ctx context.Context, _ *ToolRequest, _ approval.Constraints) (ToolResult, error) {
				close(running)
				<-ctx.Done()
				return ToolResult{}, ctx.Err()
			}
			old := newAuthorizationRig(t, &before)
			fresh := newAuthorizationRig(t, &tc.options)
			var outcomes []callOutcome
			key := hosted.start(t, func(ctx context.Context, inv durable.Invocation) error {
				rig := old
				if len(outcomes) > 0 {
					rig = fresh
				}
				outcomes = append(outcomes, outcomeOf(rig.broker.Execute(inTurn(ctx, inv), probeRequest("probe"))))
				return nil
			})
			awaitClosed(t, running)
			if _, err := hosted.db.ExecContext(t.Context(), `DELETE FROM durable_host`); err != nil {
				t.Fatalf("delete the host lease: %v", err)
			}
			hosted.awaitEnded(t, key, 2)

			if len(outcomes) != 2 || outcomes[1] != probeSucceeded {
				t.Fatalf("outcomes = %+v, want %+v in the new process", outcomes, probeSucceeded)
			}
			if prompts := old.decider.asked(); len(prompts) != tc.prompts {
				t.Errorf("decider calls before the restart = %d, want %d", len(prompts), tc.prompts)
			}
			if prompts := fresh.decider.asked(); len(prompts) != 0 {
				t.Errorf("decider calls in the new process = %d, want none", len(prompts))
			}
			if calls := fresh.probe.calls.Load(); calls != 1 {
				t.Errorf("tool runs in the new process = %d, want 1", calls)
			}
		})
	}
}

func TestTaintedApprovalOnADurableTurnFollowsReachability(t *testing.T) {
	cases := []struct {
		name      string
		reachable bool
		want      callOutcome
		prompts   int
		runs      int32
		state     string
	}{
		{name: "an approver can be reached", reachable: true, want: probeSucceeded, prompts: 1, runs: 1, state: ApprovalApproved},
		{
			name:  "nobody can be reached",
			want:  callOutcome{class: ResultUntrustedInputNoApprover, text: `untrusted_input_no_approver: tool "probe@v1" needs approval because this turn carries untrusted input, and nobody can be asked`},
			state: ApprovalMissing,
		},
	}
	hosted := startHostedRuntime(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reachable atomic.Bool
			reachable.Store(tc.reachable)
			rig := newAuthorizationRig(t, &rigOptions{sink: true, reachable: func(string) bool { return reachable.Load() }})
			request := probeRequest("probe")
			request.Tainted = true
			var first, replay callOutcome
			hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				first = outcomeOf(rig.broker.Execute(inTurn(ctx, inv), request))
				reachable.Store(!tc.reachable)
				replay = outcomeOf(rig.broker.Execute(inTurn(ctx, inv), request))
				return nil
			})
			if first != tc.want || replay != tc.want {
				t.Errorf("first = %+v, replay with the reachability reversed = %+v, want %+v for both", first, replay, tc.want)
			}
			if prompts := rig.decider.asked(); len(prompts) != tc.prompts {
				t.Errorf("decider calls = %d, want %d", len(prompts), tc.prompts)
			}
			if calls := rig.probe.calls.Load(); calls != tc.runs {
				t.Errorf("tool runs = %d, want %d", calls, tc.runs)
			}
			requireStates(t, rig.observed.list(), tc.state, tc.state)
		})
	}
}

func TestDurableToolCallWritesOneAuthorizationAction(t *testing.T) {
	cases := []struct {
		name     string
		options  rigOptions
		request  func() *ToolRequest
		state    string
		standing bool
	}{
		{name: "auto-allowed", request: func() *ToolRequest { return probeRequest("probe") }, state: ApprovalAuto},
		{name: "approved by the decider", options: rigOptions{requiresApproval: true}, request: func() *ToolRequest { return probeRequest("probe") }, state: ApprovalApproved},
		{name: "standing grant", options: rigOptions{requiresApproval: true, access: standingAccess()}, request: trustedProbeRequest, state: ApprovalStanding, standing: true},
	}
	hosted := startHostedRuntime(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAuthorizationRig(t, &tc.options)
			request := tc.request()
			canonical, err := rig.broker.canonicalRequest(request)
			if err != nil {
				t.Fatalf("canonical request: %v", err)
			}
			want := 3
			if tc.standing {
				want = 4
			}
			var outcome callOutcome
			var next string
			var positions []string
			key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				scope := durable.NewTurnScope(inv)
				outcome = outcomeOf(rig.broker.Execute(durable.WithTurnScope(ctx, scope), request))
				next = scope.NextOp()
				fresh := durable.NewTurnScope(inv)
				for range want + 1 {
					positions = append(positions, fresh.NextOp())
				}
				return nil
			})
			if outcome != probeSucceeded {
				t.Fatalf("outcome = %+v, want %+v", outcome, probeSucceeded)
			}
			rows := hosted.actionRows(t, key)
			if len(rows) != want {
				t.Fatalf("recorded actions = %d, want %d", len(rows), want)
			}
			recordedKeys := make([]string, 0, len(rows))
			for _, row := range rows {
				recordedKeys = append(recordedKeys, row.key)
			}
			if expected := slices.Sorted(slices.Values(positions[:want])); !slices.Equal(recordedKeys, expected) || next != positions[want] {
				t.Errorf("action keys = %q and then %q, want the first %d positions of the turn %q", recordedKeys, next, want, positions)
			}
			requirePolicyRow(t, rows[0])
			if tc.standing && string(rows[1].result) != "true" {
				t.Errorf("standing grant lookup = %s, want true", rows[1].result)
			}
			authorized := rows[len(rows)-2]
			record, ok := decodeAuthorization(authorized.result)
			if !ok || record.State != tc.state || record.RequestDigest != canonical.RequestDigest || record.Failure != nil {
				t.Errorf("authorization record = %s, want the state %s and the digest %s", authorized.result, tc.state, canonical.RequestDigest)
			}
			if got := journaledOutput(t, rows); got != `{"ok":true}` {
				t.Errorf("execution output = %s", got)
			}
		})
	}
}

func requirePolicyRow(t *testing.T, row actionRow) {
	t.Helper()
	recorded, err := decodeOutcome(row.result)
	if err != nil {
		t.Fatalf("policy action %s records %v, want a decision", row.key, err)
	}
	var decision approval.PolicyDecision
	if err := json.Unmarshal(recorded, &decision); err != nil || decision.Outcome == "" {
		t.Errorf("policy action %s = %s, want a decision", row.key, row.result)
	}
}

func TestDurableAuthorizationIsBoundToItsRequest(t *testing.T) {
	probe := newAuthorizationRig(t, &rigOptions{requiresApproval: true})
	own, err := probe.broker.canonicalRequest(probeRequest("probe"))
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	other, err := probe.broker.canonicalRequest(brokerRequest("probe", `{"query":"y"}`))
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	cases := []struct {
		name    string
		planted string
	}{
		{name: "another request's digest", planted: `{"state":"approved","request_digest":"` + other.RequestDigest + `"}`},
		{name: "an unknown state", planted: `{"state":"granted","request_digest":"` + own.RequestDigest + `"}`},
		{name: "the shape of another action", planted: `{"result":{}}`},
		{name: "an unknown field", planted: `{"state":"approved","request_digest":"` + own.RequestDigest + `","grant":{}}`},
		{name: "a refusal without its failure", planted: `{"state":"rejected","request_digest":"` + own.RequestDigest + `"}`},
		{name: "a failure of an unknown class", planted: `{"state":"approved","request_digest":"` + own.RequestDigest + `","failure":{"class":"maybe","detail":"x"}}`},
		{name: "trailing data", planted: `{"state":"approved","request_digest":"` + own.RequestDigest + `"} {}`},
		{name: "no record at all", planted: `null`},
	}
	hosted := startHostedRuntime(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAuthorizationRig(t, &rigOptions{requiresApproval: true})
			var outcome callOutcome
			hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				if _, err := inv.RunAction(ctx, authorizationKey(inv), func(context.Context) ([]byte, error) {
					return []byte(tc.planted), nil
				}); err != nil {
					return err
				}
				outcome = outcomeOf(rig.broker.Execute(inTurn(ctx, inv), probeRequest("probe")))
				return nil
			})
			if outcome.class != ResultPolicyDenied {
				t.Errorf("outcome = %+v, want %s", outcome, ResultPolicyDenied)
			}
			rig.requireUntouched(t)
		})
	}
}

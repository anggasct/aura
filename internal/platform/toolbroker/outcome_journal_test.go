package toolbroker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

const (
	hostedHandler       = "broker-turn"
	hostedWait          = 20 * time.Second
	hostedRequestWindow = time.Second
	journalSecret       = "s3cr3t-journal-value"
)

type hostedBody func(ctx context.Context, inv durable.Invocation) error

type hostedRuntime struct {
	db   *sql.DB
	rt   *sqlite.Runtime
	halt func() bool

	mu       sync.Mutex
	runs     int
	bodies   map[string]hostedBody
	returned map[string]int
}

func startHostedRuntime(t *testing.T) *hostedRuntime {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aura.db")
	storetest.Seed(t, path)
	db, err := store.OpenDB(t.Context(), path)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hosted := &hostedRuntime{db: db, bodies: map[string]hostedBody{}, returned: map[string]int{}}
	hosted.serve(t)
	return hosted
}

func (h *hostedRuntime) serve(t *testing.T) {
	t.Helper()
	rt, err := sqlite.New(&sqlite.Options{DB: h.db, Logger: slog.New(slog.DiscardHandler), PollInterval: 10 * time.Millisecond, HostTTL: time.Second, StopGrace: 5 * time.Second})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	rt.RegisterHandler(hostedHandler, h.handle)
	ctx, stop := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	halt := sync.OnceValue(func() bool {
		stop()
		select {
		case <-done:
			return true
		case <-time.After(hostedWait):
			return false
		}
	})
	h.rt, h.halt = rt, halt
	t.Cleanup(func() {
		if !halt() {
			t.Error("the durable host did not stop")
		}
	})
}

func (h *hostedRuntime) restartHost(t *testing.T) {
	t.Helper()
	if !h.halt() {
		t.Fatal("the durable host did not stop")
	}
	h.serve(t)
}

func (h *hostedRuntime) handle(ctx context.Context, inv durable.Invocation) error {
	key := inv.Run().Key
	h.mu.Lock()
	body := h.bodies[key]
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.returned[key]++
		h.mu.Unlock()
	}()
	return body(ctx, inv)
}

func (h *hostedRuntime) start(t *testing.T, body hostedBody) string {
	t.Helper()
	h.mu.Lock()
	h.runs++
	key := fmt.Sprintf("turn-%d", h.runs)
	h.bodies[key] = body
	h.mu.Unlock()
	if _, err := h.rt.Start(t.Context(), durable.StartRequest{Handler: hostedHandler, Key: key}); err != nil {
		t.Fatalf("start %s: %v", key, err)
	}
	return key
}

func (h *hostedRuntime) returnedCount(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.returned[key]
}

func (h *hostedRuntime) awaitEnded(t *testing.T, key string, invocations int) {
	t.Helper()
	deadline := time.NewTimer(hostedWait)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		status, err := h.rt.Status(t.Context(), durable.RunRef{Key: key})
		if err != nil {
			t.Fatalf("status of %s: %v", key, err)
		}
		if status.State != durable.RunRunning && h.returnedCount(key) == invocations {
			if status.State != durable.RunSucceeded {
				t.Fatalf("run %s ended as %s (%s), want succeeded", key, status.State, status.Detail)
			}
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting until %s ended after %d invocations (returned %d)", key, invocations, h.returnedCount(key))
		}
	}
}

func (h *hostedRuntime) run(t *testing.T, body hostedBody) string {
	t.Helper()
	key := h.start(t, body)
	h.awaitEnded(t, key, 1)
	return key
}

type actionRow struct {
	key     string
	outcome string
	result  []byte
	detail  string
}

func (h *hostedRuntime) actionRows(t *testing.T, key string) []actionRow {
	t.Helper()
	rows, err := h.db.QueryContext(t.Context(), `SELECT action_key, outcome, result, detail FROM durable_action WHERE run_key = ? ORDER BY action_key`, key)
	if err != nil {
		t.Fatalf("read the actions of %s: %v", key, err)
	}
	defer func() { _ = rows.Close() }()
	var out []actionRow
	for rows.Next() {
		var row actionRow
		if err := rows.Scan(&row.key, &row.outcome, &row.result, &row.detail); err != nil {
			t.Fatalf("scan an action of %s: %v", key, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the actions of %s: %v", key, err)
	}
	return out
}

func requireNoRowHolds(t *testing.T, rows []actionRow, value string) {
	t.Helper()
	if len(rows) == 0 {
		t.Fatal("the run recorded no action")
	}
	for _, row := range rows {
		if strings.Contains(string(row.result), value) || strings.Contains(row.detail, value) {
			t.Errorf("action %s holds %q: result %s, detail %q", row.key, value, row.result, row.detail)
		}
	}
}

func requireEveryRowIsAResult(t *testing.T, rows []actionRow) {
	t.Helper()
	for _, row := range rows {
		if row.outcome != "result" {
			t.Errorf("action %s outcome = %q (detail %q), want result", row.key, row.outcome, row.detail)
		}
	}
}

func inTurn(ctx context.Context, inv durable.Invocation) context.Context {
	return durable.WithTurnScope(ctx, durable.NewTurnScope(inv))
}

type probeTool struct {
	rule    *approval.Rule
	respond Adapter
	calls   atomic.Int32
}

func (p *probeTool) adapter(ctx context.Context, request *ToolRequest, constraints approval.Constraints) (ToolResult, error) {
	p.calls.Add(1)
	if p.respond == nil {
		return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}
	return p.respond(ctx, request, constraints)
}

func newProbeBroker(t *testing.T) *Broker {
	t.Helper()
	broker, err := New(&Options{HostCapabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return broker
}

func registerProbe(t *testing.T, broker *Broker, probe *probeTool) *probeTool {
	t.Helper()
	definition := &tools.Definition{Name: probe.rule.ToolName, Version: probe.rule.ToolVersion, Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil }}
	if err := broker.RegisterTool(definition, probe.adapter, probe.rule); err != nil {
		t.Fatalf("register %s: %v", probe.rule.ToolName, err)
	}
	return probe
}

func probeRule(name string) *approval.Rule {
	return &approval.Rule{ToolName: name, ToolVersion: "v1"}
}

func probeRequest(name string) *ToolRequest {
	return brokerRequest(name, `{"query":"x"}`)
}

type callOutcome struct {
	class  ResultClass
	text   string
	output string
}

func outcomeOf(result ToolResult, err error) callOutcome {
	if err != nil {
		class, _ := CodeOf(err)
		return callOutcome{class: class, text: err.Error()}
	}
	return callOutcome{class: result.Class, output: string(result.Output)}
}

type failureBranch struct {
	name  string
	err   error
	class ResultClass
	text  string
}

func failureBranches(detail string) []failureBranch {
	return []failureBranch{
		{
			name:  "coded broker error",
			err:   errorf(ResultPolicyDenied, "denied %s", detail),
			class: ResultPolicyDenied,
			text:  "policy_denied: policy_denied: denied " + detail,
		},
		{
			name:  "sandbox path denial",
			err:   sandbox.Errorf(sandbox.ErrorCodeSandboxPathDenied, "path %s is outside the workspace", detail),
			class: ResultPolicyDenied,
			text:  "policy_denied: sandbox_path_denied: path " + detail + " is outside the workspace",
		},
		{
			name:  "sandbox unavailable",
			err:   sandbox.Errorf(sandbox.ErrorCodeSandboxUnavailable, "start child: %s", detail),
			class: ResultCapabilityUnavailable,
			text:  "capability_unavailable: sandbox_unavailable: start child: " + detail,
		},
		{
			name:  "egress denial",
			err:   egress.Errorf(egress.ErrorCodeEgressDenied, "cannot resolve %s", detail),
			class: ResultPolicyDenied,
			text:  "policy_denied: egress_denied: cannot resolve " + detail,
		},
		{
			name:  "uncoded error",
			err:   fmt.Errorf("upstream said %s", detail),
			class: ResultExecutionFailed,
			text:  `execution_failed: tool "probe@v1" failed: upstream said ` + detail,
		},
	}
}

func failingWith(err error) Adapter {
	return func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{}, err
	}
}

func waitPast(deadline time.Time) {
	for time.Now().Before(deadline) {
		<-time.After(time.Until(deadline))
	}
}

type pathCase struct {
	name    string
	respond Adapter
	window  bool
	class   ResultClass
	text    string
}

func TestToolFailureIsTheSameOnEveryPath(t *testing.T) {
	var cases []pathCase
	for _, branch := range failureBranches("probe-detail") {
		cases = append(cases, pathCase{name: branch.name, respond: failingWith(branch.err), class: branch.class, text: branch.text})
	}
	cases = append(cases,
		pathCase{
			name:    "own timeout of the tool",
			respond: failingWith(context.DeadlineExceeded),
			class:   ResultDeadlineExceeded,
			text:    "deadline_exceeded: tool request ended: context deadline exceeded",
		},
		pathCase{
			name: "request deadline passes during the tool",
			respond: func(_ context.Context, request *ToolRequest, _ approval.Constraints) (ToolResult, error) {
				waitPast(request.Deadline)
				return ToolResult{}, errors.New("upstream gave up")
			},
			window: true,
			class:  ResultDeadlineExceeded,
			text:   "deadline_exceeded: tool request deadline exceeded",
		},
	)
	hosted := startHostedRuntime(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := newProbeBroker(t)
			counted := registerProbe(t, broker, &probeTool{rule: probeRule("probe"), respond: tc.respond})
			request := func() *ToolRequest {
				request := probeRequest("probe")
				if tc.window {
					request.Deadline = time.Now().Add(hostedRequestWindow)
				}
				return request
			}
			want := callOutcome{class: tc.class, text: tc.text}

			if got := outcomeOf(broker.Execute(t.Context(), request())); got != want {
				t.Errorf("in memory = %+v, want %+v", got, want)
			}
			before := counted.calls.Load()
			var first, replay callOutcome
			hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				durableRequest := request()
				first = outcomeOf(broker.Execute(inTurn(ctx, inv), durableRequest))
				replay = outcomeOf(broker.Execute(inTurn(ctx, inv), durableRequest))
				return nil
			})
			if first != want {
				t.Errorf("durable first execution = %+v, want %+v", first, want)
			}
			if replay != want {
				t.Errorf("durable replay = %+v, want %+v", replay, want)
			}
			if ran := counted.calls.Load() - before; ran != 1 {
				t.Errorf("adapter calls on the durable path = %d, want 1", ran)
			}
		})
	}
}

type endingContext struct {
	end context.CancelFunc
}

func (e *endingContext) within(ctx context.Context) context.Context {
	ended, cancel := context.WithCancel(ctx)
	e.end = cancel
	return ended
}

func (e *endingContext) thenReturn(result ToolResult, err error) Adapter {
	return func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		e.end()
		return result, err
	}
}

func TestDurableToolFailureIsJournaledRedacted(t *testing.T) {
	hosted := startHostedRuntime(t)
	ownTimeout := failureBranch{
		name:  "the tool's own timeout",
		err:   fmt.Errorf("fetch with %s: %w", journalSecret, context.DeadlineExceeded),
		class: ResultDeadlineExceeded,
	}
	t.Run("a tool whose context ended", func(t *testing.T) {
		broker := newProbeBroker(t)
		broker.RegisterSecret(journalSecret)
		ending := &endingContext{}
		registerProbe(t, broker, &probeTool{rule: probeRule("probe"), respond: ending.thenReturn(ToolResult{}, errors.New("stopped while sending "+journalSecret))})
		var first callOutcome
		key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
			first = outcomeOf(broker.Execute(ending.within(inTurn(ctx, inv)), probeRequest("probe")))
			return nil
		})
		if first.class != ResultDeadlineExceeded {
			t.Errorf("class = %q, want %q", first.class, ResultDeadlineExceeded)
		}
		if strings.Contains(first.text, journalSecret) {
			t.Errorf("returned text = %q, want the secret redacted", first.text)
		}
		rows := hosted.actionRows(t, key)
		requireNoRowHolds(t, rows, journalSecret)
		if execution := rows[len(rows)-1]; execution.outcome != "failed" || !strings.Contains(execution.detail, "[REDACTED]") {
			t.Errorf("execution action = %q with the detail %q, want a failed action with redacted text", execution.outcome, execution.detail)
		}
	})
	for _, branch := range append(failureBranches(journalSecret), ownTimeout) {
		t.Run(branch.name, func(t *testing.T) {
			broker := newProbeBroker(t)
			broker.RegisterSecret(journalSecret)
			registerProbe(t, broker, &probeTool{rule: probeRule("probe"), respond: failingWith(branch.err)})
			var first callOutcome
			key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				first = outcomeOf(broker.Execute(inTurn(ctx, inv), probeRequest("probe")))
				return nil
			})
			if first.class != branch.class {
				t.Errorf("class = %q, want %q", first.class, branch.class)
			}
			if strings.Contains(first.text, journalSecret) || !strings.Contains(first.text, "[REDACTED]") {
				t.Errorf("returned text = %q, want the secret redacted", first.text)
			}
			rows := hosted.actionRows(t, key)
			if len(rows) != 3 {
				t.Fatalf("recorded actions = %d, want 3 (policy, authorization, execution)", len(rows))
			}
			requireNoRowHolds(t, rows, journalSecret)
			requireEveryRowIsAResult(t, rows)
		})
	}
}

func TestDurableToolResultIsJournaledRedacted(t *testing.T) {
	cases := []struct {
		name         string
		registerLate bool
	}{
		{name: "secret registered before the call"},
		{name: "secret registered by the adapter during its execution", registerLate: true},
	}
	hosted := startHostedRuntime(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := newProbeBroker(t)
			if !tc.registerLate {
				broker.RegisterSecret(journalSecret)
			}
			counted := registerProbe(t, broker, &probeTool{rule: probeRule("probe"), respond: func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				if tc.registerLate {
					broker.RegisterSecret(journalSecret)
				}
				return ToolResult{Output: json.RawMessage(`{"token":"` + journalSecret + `"}`)}, nil
			}})
			var first, replay callOutcome
			key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				first = outcomeOf(broker.Execute(inTurn(ctx, inv), probeRequest("probe")))
				replay = outcomeOf(broker.Execute(inTurn(ctx, inv), probeRequest("probe")))
				return nil
			})
			want := callOutcome{class: ResultOK, output: `{"token":"[REDACTED]"}`}
			if first != want {
				t.Errorf("first execution = %+v, want %+v", first, want)
			}
			if replay != want {
				t.Errorf("replay = %+v, want %+v", replay, want)
			}
			if calls := counted.calls.Load(); calls != 1 {
				t.Errorf("adapter calls = %d, want 1", calls)
			}
			rows := hosted.actionRows(t, key)
			requireNoRowHolds(t, rows, journalSecret)
			execution := rows[len(rows)-1]
			if !strings.Contains(string(execution.result), "[REDACTED]") {
				t.Errorf("recorded tool result = %s, want the redacted output", execution.result)
			}
		})
	}
	t.Run("a tool that succeeded although its context ended", func(t *testing.T) {
		broker := newProbeBroker(t)
		broker.RegisterSecret(journalSecret)
		ending := &endingContext{}
		registerProbe(t, broker, &probeTool{rule: probeRule("probe"), respond: ending.thenReturn(ToolResult{Output: json.RawMessage(`{"token":"` + journalSecret + `"}`)}, nil)})
		var first callOutcome
		key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
			first = outcomeOf(broker.Execute(ending.within(inTurn(ctx, inv)), probeRequest("probe")))
			return nil
		})
		if want := (callOutcome{class: ResultOK, output: `{"token":"[REDACTED]"}`}); first != want {
			t.Errorf("first execution = %+v, want %+v", first, want)
		}
		rows := hosted.actionRows(t, key)
		requireNoRowHolds(t, rows, journalSecret)
		requireEveryRowIsAResult(t, rows)
	})
}

func TestDurableSecretRegisteredLaterIsRedactedOnReplay(t *testing.T) {
	const later = "rotated-later-value"
	cases := []struct {
		name    string
		respond Adapter
		want    callOutcome
	}{
		{
			name: "tool output",
			respond: func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				return ToolResult{Output: json.RawMessage(`{"echo":"` + later + `"}`)}, nil
			},
			want: callOutcome{class: ResultOK, output: `{"echo":"[REDACTED]"}`},
		},
		{
			name:    "tool error text",
			respond: failingWith(errors.New("upstream said " + later)),
			want:    callOutcome{class: ResultExecutionFailed, text: `execution_failed: tool "probe@v1" failed: upstream said [REDACTED]`},
		},
	}
	hosted := startHostedRuntime(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := newProbeBroker(t)
			counted := registerProbe(t, broker, &probeTool{rule: probeRule("probe"), respond: tc.respond})
			var first, replay callOutcome
			hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				first = outcomeOf(broker.Execute(inTurn(ctx, inv), probeRequest("probe")))
				broker.RegisterSecret(later)
				replay = outcomeOf(broker.Execute(inTurn(ctx, inv), probeRequest("probe")))
				return nil
			})
			if !strings.Contains(first.output+first.text, later) {
				t.Fatalf("first execution = %+v, want the value before it was registered", first)
			}
			if replay != tc.want {
				t.Errorf("replay = %+v, want %+v", replay, tc.want)
			}
			if calls := counted.calls.Load(); calls != 1 {
				t.Errorf("adapter calls = %d, want 1", calls)
			}
		})
	}
}

func TestDurableToolOutputThatIsNotJSONIsARecordedFailure(t *testing.T) {
	hosted := startHostedRuntime(t)
	broker := newProbeBroker(t)
	counted := registerProbe(t, broker, &probeTool{rule: probeRule("probe"), respond: func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{Output: json.RawMessage(`not json`)}, nil
	}})
	var first, replay callOutcome
	key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
		first = outcomeOf(broker.Execute(inTurn(ctx, inv), probeRequest("probe")))
		replay = outcomeOf(broker.Execute(inTurn(ctx, inv), probeRequest("probe")))
		return nil
	})
	want := callOutcome{class: ResultExecutionFailed, text: `execution_failed: tool "probe@v1" returned output that is not valid JSON`}
	if first != want {
		t.Errorf("first execution = %+v, want %+v", first, want)
	}
	if replay != want {
		t.Errorf("replay = %+v, want %+v", replay, want)
	}
	if calls := counted.calls.Load(); calls != 1 {
		t.Errorf("adapter calls = %d, want 1", calls)
	}
	requireEveryRowIsAResult(t, hosted.actionRows(t, key))
}

type invocationOutcome struct {
	first  callOutcome
	second callOutcome
}

func TestDurableToolOutcomeSurvivesBeingInvokedAgain(t *testing.T) {
	cases := []struct {
		name      string
		interrupt func(ctx context.Context) (ToolResult, error)
	}{
		{
			name: "interruption reported as a coded error",
			interrupt: func(ctx context.Context) (ToolResult, error) {
				return ToolResult{}, errorf(ResultExecutionFailed, "tool call failed: %v", ctx.Err())
			},
		},
		{
			name: "interruption reported as a result with a failure class",
			interrupt: func(context.Context) (ToolResult, error) {
				return ToolResult{Class: ResultDeadlineExceeded, Output: json.RawMessage(`{"status":"cancelled"}`)}, nil
			},
		},
	}
	hosted := startHostedRuntime(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := newProbeBroker(t)
			first := registerProbe(t, broker, &probeTool{rule: probeRule("first"), respond: failingWith(errorf(ResultPolicyDenied, "denied first"))})
			waiting := make(chan struct{})
			second := &probeTool{rule: probeRule("second")}
			second.respond = func(ctx context.Context, _ *ToolRequest, _ approval.Constraints) (ToolResult, error) {
				if second.calls.Load() > 1 {
					return ToolResult{Output: json.RawMessage(`{"done":true}`)}, nil
				}
				close(waiting)
				<-ctx.Done()
				return tc.interrupt(ctx)
			}
			registerProbe(t, broker, second)
			var outcomes []invocationOutcome
			key := hosted.start(t, func(ctx context.Context, inv durable.Invocation) error {
				ctx = inTurn(ctx, inv)
				var outcome invocationOutcome
				outcome.first = outcomeOf(broker.Execute(ctx, probeRequest("first")))
				outcome.second = outcomeOf(broker.Execute(ctx, probeRequest("second")))
				outcomes = append(outcomes, outcome)
				return nil
			})
			select {
			case <-waiting:
			case <-time.After(hostedWait):
				t.Fatal("the second tool never started")
			}
			if _, err := hosted.db.ExecContext(t.Context(), `DELETE FROM durable_host`); err != nil {
				t.Fatalf("delete the host lease: %v", err)
			}
			hosted.awaitEnded(t, key, 2)

			if len(outcomes) != 2 {
				t.Fatalf("invocations = %d, want 2", len(outcomes))
			}
			if calls := first.calls.Load(); calls != 1 {
				t.Errorf("first tool calls = %d, want 1", calls)
			}
			wantFirst := callOutcome{class: ResultPolicyDenied, text: "policy_denied: policy_denied: denied first"}
			for i, outcome := range outcomes {
				if outcome.first != wantFirst {
					t.Errorf("first tool in invocation %d = %+v, want %+v", i+1, outcome.first, wantFirst)
				}
			}
			if calls := second.calls.Load(); calls != 2 {
				t.Errorf("second tool calls = %d, want 2", calls)
			}
			if want := (callOutcome{class: ResultOK, output: `{"done":true}`}); outcomes[1].second != want {
				t.Errorf("second tool in the last invocation = %+v, want %+v", outcomes[1].second, want)
			}
			requireNoInterruptionRecorded(t, hosted.actionRows(t, key))
		})
	}
}

func requireNoInterruptionRecorded(t *testing.T, rows []actionRow) {
	t.Helper()
	failures, completions := 0, 0
	for _, row := range rows {
		if row.outcome != "result" {
			t.Errorf("action %s outcome = %q (detail %q), want result", row.key, row.outcome, row.detail)
			continue
		}
		if authorized, ok := decodeAuthorization(row.result); ok {
			if authorized.Failure != nil {
				t.Errorf("action %s records the refused authorization %+v", row.key, authorized.Failure)
			}
			continue
		}
		recorded, err := decodeOutcome(row.result)
		if err != nil {
			failures++
			if code, _ := CodeOf(err); code != ResultPolicyDenied {
				t.Errorf("action %s records the failure %v, want only the first tool's denial", row.key, err)
			}
			continue
		}
		var result struct {
			Class  ResultClass     `json:"class"`
			Output json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(recorded, &result); err != nil {
			t.Fatalf("decode action %s: %v", row.key, err)
		}
		if result.Class != "" && result.Class != ResultOK {
			t.Errorf("action %s records a result with the class %s", row.key, result.Class)
		}
		if string(result.Output) == `{"done":true}` {
			completions++
		}
	}
	if failures != 1 || completions != 1 {
		t.Errorf("recorded failures = %d and completions of the second tool = %d, want 1 and 1", failures, completions)
	}
}

type approvalFixture struct {
	broker *Broker
	probe  *ToolRequest
	gated  *ToolRequest
}

func newApprovalFixture(t *testing.T) *approvalFixture {
	t.Helper()
	broker := newProbeBroker(t)
	registerProbe(t, broker, &probeTool{rule: probeRule("probe")})
	registerProbe(t, broker, &probeTool{rule: &approval.Rule{ToolName: "gated", ToolVersion: "v1", RequiredCapabilities: []string{"remote-unicorns"}}})
	fixture := &approvalFixture{broker: broker}
	for name, target := range map[string]**ToolRequest{"probe": &fixture.probe, "gated": &fixture.gated} {
		canonical, err := broker.canonicalRequest(probeRequest(name))
		if err != nil {
			t.Fatalf("canonical request of %s: %v", name, err)
		}
		*target = &canonical
	}
	return fixture
}

var foreignGrant = approval.ApprovalGrant{GrantID: "grant-1", Nonce: "nonce-1", PrincipalID: "someone-else"}

type approvalCase struct {
	name  string
	call  func(ctx context.Context, fixture *approvalFixture) error
	class ResultClass
}

func TestDurableApprovalErrorsReplayWithTheirClass(t *testing.T) {
	cases := []approvalCase{
		{
			name: "policy evaluation without a host capability",
			call: func(ctx context.Context, fixture *approvalFixture) error {
				_, err := fixture.broker.Execute(ctx, probeRequest("gated"))
				return err
			},
			class: ResultCapabilityUnavailable,
		},
		{
			name: "validation of a grant for another request",
			call: func(ctx context.Context, fixture *approvalFixture) error {
				scope, _ := durable.TurnScopeFrom(ctx)
				grant := foreignGrant
				return fixture.broker.validateDurable(ctx, scope, fixture.probe, &grant)
			},
			class: ResultPolicyDenied,
		},
		{
			name: "authorization with an expiry that has passed",
			call: func(ctx context.Context, fixture *approvalFixture) error {
				_, err := fixture.broker.authorize(ctx, &authorization{canonical: fixture.probe, decision: approval.PolicyDecision{Outcome: approval.OutcomeAllow}, expiry: time.Now().Add(-time.Minute)})
				return err
			},
			class: ResultDeadlineExceeded,
		},
		{
			name: "authorization of a call that needs approval with nobody to ask",
			call: func(ctx context.Context, fixture *approvalFixture) error {
				_, err := fixture.broker.authorize(ctx, &authorization{canonical: fixture.probe, decision: approval.PolicyDecision{Outcome: approval.OutcomeRequireApproval}, expiry: time.Now().Add(time.Minute)})
				return err
			},
			class: ResultApprovalRequired,
		},
	}
	hosted := startHostedRuntime(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newApprovalFixture(t)
			var first, replay callOutcome
			key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				first = outcomeOf(ToolResult{}, tc.call(inTurn(ctx, inv), fixture))
				replay = outcomeOf(ToolResult{}, tc.call(inTurn(ctx, inv), fixture))
				return nil
			})
			if first.class != tc.class || first.text == "" {
				t.Errorf("first call = %+v, want the class %s", first, tc.class)
			}
			if replay != first {
				t.Errorf("replay = %+v, want %+v", replay, first)
			}
			rows := hosted.actionRows(t, key)
			if len(rows) != 1 {
				t.Fatalf("recorded actions = %d, want 1", len(rows))
			}
			requireEveryRowIsAResult(t, rows)
		})
	}
	t.Run("a recorded approval failure is redacted", func(t *testing.T) {
		const capabilityName = "remote-unicorns"
		fixture := newApprovalFixture(t)
		fixture.broker.RegisterSecret(capabilityName)
		_, inMemory := fixture.broker.Execute(t.Context(), probeRequest("gated"))
		if inMemory == nil || !strings.Contains(inMemory.Error(), capabilityName) {
			t.Fatalf("in-memory error = %v, want it to name %s", inMemory, capabilityName)
		}
		var first callOutcome
		key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
			_, err := fixture.broker.Execute(inTurn(ctx, inv), probeRequest("gated"))
			first = outcomeOf(ToolResult{}, err)
			return nil
		})
		if first.class != ResultCapabilityUnavailable || strings.Contains(first.text, capabilityName) {
			t.Errorf("first call = %+v, want %s without %s", first, ResultCapabilityUnavailable, capabilityName)
		}
		rows := hosted.actionRows(t, key)
		requireNoRowHolds(t, rows, capabilityName)
		requireEveryRowIsAResult(t, rows)
	})
	ended := []struct {
		name    string
		request func(fixture *approvalFixture) *ToolRequest
	}{
		{name: "validation with an ended context", request: func(fixture *approvalFixture) *ToolRequest { return fixture.probe }},
		{name: "validation of a coded engine failure with an ended context", request: func(fixture *approvalFixture) *ToolRequest { return fixture.gated }},
	}
	for _, tc := range ended {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newApprovalFixture(t)
			var err error
			key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				endedCtx, cancel := context.WithCancel(inTurn(ctx, inv))
				cancel()
				scope, _ := durable.TurnScopeFrom(endedCtx)
				grant := foreignGrant
				err = fixture.broker.validateDurable(endedCtx, scope, tc.request(fixture), &grant)
				return nil
			})
			if code, ok := sqlite.CodeOf(err); !ok || code != sqlite.ErrorCodeActionFailed {
				t.Errorf("error = %v, want the runtime's failed action", err)
			}
			if code, ok := CodeOf(err); ok {
				t.Errorf("error carries the broker class %s, want it raw", code)
			}
			rows := hosted.actionRows(t, key)
			if len(rows) != 1 || rows[0].outcome != "failed" {
				t.Fatalf("recorded actions = %+v, want one failed action", rows)
			}
		})
	}
	interrupted := []struct {
		name   string
		answer func(end context.CancelFunc) (bool, error)
		detail string
	}{
		{
			name: "authorization whose decider fails after its context ended",
			answer: func(end context.CancelFunc) (bool, error) {
				end()
				return false, fmt.Errorf("card for %s: %w", journalSecret, context.Canceled)
			},
			detail: "[REDACTED]",
		},
		{
			name: "authorization answered after its context ended",
			answer: func(end context.CancelFunc) (bool, error) {
				end()
				return true, nil
			},
			detail: "tool request ended",
		},
	}
	for _, tc := range interrupted {
		t.Run(tc.name, func(t *testing.T) {
			ending := &endingContext{}
			var asked atomic.Int32
			broker, err := New(&Options{HostCapabilities: capabilitytest.Host(t), ApprovalDecider: func(context.Context, *ApprovalPrompt) (bool, error) {
				asked.Add(1)
				return tc.answer(ending.end)
			}})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			broker.RegisterSecret(journalSecret)
			registerProbe(t, broker, &probeTool{rule: &approval.Rule{ToolName: "probe", ToolVersion: "v1", RequiresApproval: true}})
			canonical, err := broker.canonicalRequest(probeRequest("probe"))
			if err != nil {
				t.Fatalf("canonical request: %v", err)
			}
			call := &authorization{canonical: &canonical, definition: broker.definitions["probe@v1"], decision: approval.PolicyDecision{Outcome: approval.OutcomeRequireApproval}, expiry: time.Now().Add(time.Minute)}
			var first, replay callOutcome
			key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				_, err := broker.authorize(ending.within(inTurn(ctx, inv)), call)
				first = outcomeOf(ToolResult{}, err)
				_, err = broker.authorize(inTurn(ctx, inv), call)
				replay = outcomeOf(ToolResult{}, err)
				return nil
			})
			if first.class != ResultDeadlineExceeded || strings.Contains(first.text, journalSecret) {
				t.Errorf("first call = %+v, want %s without the secret", first, ResultDeadlineExceeded)
			}
			if replay.class != ResultPolicyDenied || strings.Contains(replay.text, journalSecret) {
				t.Errorf("replay = %+v, want %s without the secret", replay, ResultPolicyDenied)
			}
			if n := asked.Load(); n != 1 {
				t.Errorf("decider calls = %d, want 1", n)
			}
			rows := hosted.actionRows(t, key)
			if len(rows) != 1 || rows[0].outcome != "failed" || !strings.Contains(rows[0].detail, tc.detail) {
				t.Fatalf("recorded actions = %+v, want one failed action whose detail holds %q", rows, tc.detail)
			}
			requireNoRowHolds(t, rows, journalSecret)
		})
	}
}

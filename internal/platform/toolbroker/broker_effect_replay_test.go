package toolbroker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

const (
	replayArguments      = `{"command":["true"]}`
	unavailablePhrase    = "effectful tool result is unavailable for a replayed request"
	mismatchedDigestText = "does not match its digest"
)

type effectOutcomes struct {
	mu       sync.Mutex
	outcomes []string
}

func (o *effectOutcomes) Observe(_ context.Context, observation *effect.Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.outcomes = append(o.outcomes, observation.Outcome)
}

func (o *effectOutcomes) count(outcome string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, got := range o.outcomes {
		if got == outcome {
			n++
		}
	}
	return n
}

type replayConfig struct {
	output      json.RawMessage
	inlineLimit int64
	unlimited   bool
	artifacts   bool
	secrets     []string
	decider     ApprovalDecider
}

type replayHarness struct {
	broker   *Broker
	executor *effect.Executor
	db       *sql.DB
	observed *effectOutcomes
	output   json.RawMessage
	calls    int
}

func newReplayHarness(t *testing.T, config *replayConfig) *replayHarness {
	t.Helper()
	db := storetest.OpenMigrated(t)
	now := time.Now().UTC()
	for _, id := range []string{"session-1", "session-2"} {
		if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: id, OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create session %s: %v", id, err)
		}
	}
	observed := &effectOutcomes{}
	journal, err := effect.NewJournal(db, effect.Options{Logger: slog.New(slog.DiscardHandler), Observer: observed})
	if err != nil {
		t.Fatalf("new journal: %v", err)
	}
	executor, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	harness := &replayHarness{executor: executor, db: db, observed: observed, output: config.output}
	options := &Options{
		HostCapabilities:     capabilitytest.Host(t),
		Effects:              executor,
		MaxInlineResultBytes: config.inlineLimit,
		Secrets:              config.secrets,
		ApprovalDecider:      config.decider,
		Adapters: map[string]Adapter{
			"exec@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				harness.calls++
				return ToolResult{Output: harness.output}, nil
			},
		},
	}
	if config.unlimited {
		policy := DefaultPolicy()
		rule := policy.Rules["exec"]
		rule.Constraints.MaxOutputBytes = 0
		policy.Rules["exec"] = rule
		options.Policy = policy
	}
	if config.artifacts {
		options.Artifacts = store.NewArtifactStore(db, filepath.Join(t.TempDir(), "artifacts"), 8<<20)
	}
	broker, err := New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	harness.broker = broker
	return harness
}

func replayRequest(arguments string) *ToolRequest {
	request := brokerRequest("exec", arguments, "exec-linux")
	request.EventSequence = 1
	return request
}

func (h *replayHarness) run(ctx context.Context, request *ToolRequest) (ToolResult, error) {
	grant, err := h.broker.Grant(ctx, request, time.Minute)
	if err != nil {
		return ToolResult{}, err
	}
	request.Approval = &grant
	return h.broker.Execute(ctx, request)
}

func (h *replayHarness) mustRun(t *testing.T, request *ToolRequest) ToolResult {
	t.Helper()
	result, err := h.run(t.Context(), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return result
}

func (h *replayHarness) storedRecord(t *testing.T) []byte {
	t.Helper()
	var record sql.NullString
	if err := h.db.QueryRowContext(t.Context(), `SELECT result_json FROM effect_intent`).Scan(&record); err != nil {
		t.Fatalf("read the recorded result: %v", err)
	}
	if !record.Valid {
		t.Fatal("the intent holds no recorded result")
	}
	return []byte(record.String)
}

func (h *replayHarness) intentState(t *testing.T) effect.State {
	t.Helper()
	var state effect.State
	if err := h.db.QueryRowContext(t.Context(), `SELECT state FROM effect_intent`).Scan(&state); err != nil {
		t.Fatalf("read the intent state: %v", err)
	}
	return state
}

func requireSameResult(t *testing.T, replay, first *ToolResult) {
	t.Helper()
	if !bytes.Equal(first.Output, replay.Output) || first.Class != replay.Class || first.Truncated != replay.Truncated || first.Untrusted != replay.Untrusted {
		t.Fatalf("replay = %s %s %v %v, want %s %s %v %v", replay.Output, replay.Class, replay.Truncated, replay.Untrusted, first.Output, first.Class, first.Truncated, first.Untrusted)
	}
}

func requireDetailSuffix(t *testing.T, err error, class ResultClass, suffix string) {
	t.Helper()
	var coded *Error
	if !errors.As(err, &coded) || coded.Class != class || !strings.HasSuffix(coded.Detail, suffix) {
		t.Fatalf("error = %v, want class %s ending with %q", err, class, suffix)
	}
}

func lessThanHeavyOutput(size int) json.RawMessage {
	return json.RawMessage(`"` + strings.Repeat("<", size-2) + `"`)
}

func TestBrokerReplayOfASucceededEffectReturnsItsRecordedResult(t *testing.T) {
	probe := &ToolResult{ToolName: "exec", ToolVersion: "v1", Class: ResultOK, Untrusted: true}
	recordLimit, err := recordOutputLimit(probe)
	if err != nil {
		t.Fatalf("recordOutputLimit: %v", err)
	}
	const secret = "replay-secret-value"
	spaced := json.RawMessage("{ \"a\" : \"<&>\",\n  \"token\" : \"" + secret + "\", \"n\" : [ 1, 2 ] }")
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, []byte(strings.ReplaceAll(string(spaced), secret, "[REDACTED]"))); err != nil {
		t.Fatalf("compact the expected output: %v", err)
	}
	cases := []struct {
		name          string
		config        replayConfig
		wantCompact   string
		wantTruncated bool
		wantArtifacts int
		wantRecordLen int
	}{
		{
			name:        "whitespace, markup characters and a registered secret",
			config:      replayConfig{output: spaced, inlineLimit: 64 << 10, secrets: []string{secret}},
			wantCompact: compacted.String(),
		},
		{
			name:          "an output above the inline limit without an artifact store",
			config:        replayConfig{output: oversizedOutput(8192), inlineLimit: 4096},
			wantTruncated: true,
		},
		{
			name:          "an output above the inline limit with an artifact store",
			config:        replayConfig{output: oversizedOutput(8192), inlineLimit: 4096, artifacts: true},
			wantTruncated: true,
			wantArtifacts: 1,
		},
		{
			name:          "a markup-heavy output of exactly the record limit",
			config:        replayConfig{output: lessThanHeavyOutput(int(recordLimit)), inlineLimit: 4 << 20, unlimited: true},
			wantRecordLen: effect.MaxResultBytes,
		},
		{
			name:          "a markup-heavy output one byte above the record limit",
			config:        replayConfig{output: lessThanHeavyOutput(int(recordLimit) + 1), inlineLimit: 4 << 20, unlimited: true},
			wantTruncated: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newReplayHarness(t, &tc.config)
			first := harness.mustRun(t, replayRequest(replayArguments))
			if first.Truncated != tc.wantTruncated {
				t.Fatalf("first truncated = %v, want %v", first.Truncated, tc.wantTruncated)
			}
			record := harness.storedRecord(t)
			if tc.wantRecordLen != 0 && len(record) != tc.wantRecordLen {
				t.Fatalf("recorded result bytes = %d, want %d", len(record), tc.wantRecordLen)
			}
			recorded, err := decodeRecord(record)
			if err != nil {
				t.Fatalf("decode the recorded result: %v", err)
			}
			if !bytes.Equal(first.Output, recorded.Output) {
				t.Fatalf("first output = %s, want the recorded output %s", first.Output, recorded.Output)
			}
			if tc.wantCompact != "" && string(first.Output) != tc.wantCompact {
				t.Fatalf("first output = %s, want the compacted adapter output %s", first.Output, tc.wantCompact)
			}
			replay, err := harness.run(t.Context(), replayRequest(replayArguments))
			if err != nil {
				t.Fatalf("replay = %v, want %s %s %v %v", err, first.Output, first.Class, first.Truncated, first.Untrusted)
			}
			requireSameResult(t, &replay, &first)
			if tc.config.artifacts {
				var artifacts int
				if err := harness.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM artifact_ref`).Scan(&artifacts); err != nil {
					t.Fatalf("count artifacts: %v", err)
				}
				if artifacts != tc.wantArtifacts {
					t.Fatalf("artifacts = %d, want %d", artifacts, tc.wantArtifacts)
				}
			}
			if harness.calls != 1 {
				t.Fatalf("adapter calls = %d, want 1", harness.calls)
			}
		})
	}
}

func TestBrokerRecordedEffectResultHoldsNoSecret(t *testing.T) {
	const before, later = "secret-registered-before", "value-registered-after"
	harness := newReplayHarness(t, &replayConfig{
		output:      json.RawMessage(`{"token":"` + before + `","later":"` + later + `"}`),
		inlineLimit: 64 << 10,
		secrets:     []string{before},
	})
	harness.mustRun(t, replayRequest(replayArguments))
	if record := harness.storedRecord(t); strings.Contains(string(record), before) {
		t.Fatalf("recorded result holds a registered secret: %s", record)
	}
	harness.broker.RegisterSecret(later)
	replay := harness.mustRun(t, replayRequest(replayArguments))
	if want := `{"token":"[REDACTED]","later":"[REDACTED]"}`; string(replay.Output) != want {
		t.Fatalf("replay output = %s, want %s", replay.Output, want)
	}
}

func TestBrokerRefusesAnEffectReplayItCannotServe(t *testing.T) {
	succeededWithoutRecord := func(t *testing.T, harness *replayHarness) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"tool_name": "exec", "tool_version": "v1", "arguments": json.RawMessage(replayArguments)})
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		_, err = harness.executor.Execute(t.Context(), &effect.PrepareRequest{
			SessionID: "session-1", TurnID: "turn-1", ToolCallID: "request-1", IdempotencyKey: "idempotency-1",
			Provider: "builtin-tools", Operation: "exec", Classification: effect.ClassificationEffectful,
			Request: payload, EventSequence: 1,
		}, &succeedingProvider{})
		if err != nil {
			t.Fatalf("seed a succeeded intent: %v", err)
		}
	}
	tamper := func(t *testing.T, harness *replayHarness) {
		t.Helper()
		record := harness.storedRecord(t)
		changed := strings.Replace(string(record), `"output":{"ok":true}`, `"output":{"ok":false}`, 1)
		if changed == string(record) {
			t.Fatalf("the recorded result %s has no output to change", record)
		}
		if _, err := harness.db.ExecContext(t.Context(), `UPDATE effect_intent SET result_json = ?`, changed); err != nil {
			t.Fatalf("tamper with the record: %v", err)
		}
	}
	cases := []struct {
		name       string
		output     json.RawMessage
		seed       func(t *testing.T, harness *replayHarness)
		afterFirst func(t *testing.T, harness *replayHarness)
		replay     func() *ToolRequest
		wantClass  ResultClass
		wantSuffix string
		wantCalls  int
		wantUnrec  int
	}{
		{
			name: "a different request under the key", output: json.RawMessage(`{"ok":true}`),
			replay:    func() *ToolRequest { return replayRequest(`{"command":["false"]}`) },
			wantClass: ResultInvalidArgument, wantCalls: 1,
		},
		{
			name: "a succeeded intent without a record", output: json.RawMessage(`{"ok":true}`),
			seed:      succeededWithoutRecord,
			replay:    func() *ToolRequest { return replayRequest(replayArguments) },
			wantClass: ResultExecutionFailed, wantSuffix: unavailablePhrase, wantCalls: 0,
		},
		{
			name: "a tampered record", output: json.RawMessage(`{"ok":true}`),
			afterFirst: tamper,
			replay:     func() *ToolRequest { return replayRequest(replayArguments) },
			wantClass:  ResultExecutionFailed, wantSuffix: mismatchedDigestText, wantCalls: 1,
		},
		{
			name: "the same key and request from another session", output: json.RawMessage(`{"ok":true}`),
			replay: func() *ToolRequest {
				request := replayRequest(replayArguments)
				request.SessionID = "session-2"
				return request
			},
			wantClass: ResultInvalidArgument, wantCalls: 1,
		},
		{
			name: "an adapter output that is not JSON", output: json.RawMessage(`not json at all`),
			replay:    func() *ToolRequest { return replayRequest(replayArguments) },
			wantClass: ResultExecutionFailed, wantSuffix: unavailablePhrase, wantCalls: 1, wantUnrec: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			harness := newReplayHarness(t, &replayConfig{output: tc.output, inlineLimit: 64 << 10})
			if tc.seed != nil {
				tc.seed(t, harness)
			} else {
				first, err := harness.run(t.Context(), replayRequest(replayArguments))
				if state := harness.intentState(t); state != effect.StateSucceeded {
					t.Fatalf("intent state = %s, want succeeded", state)
				}
				if err != nil {
					t.Fatalf("first Execute: %v", err)
				}
				if tc.wantUnrec != 0 && !bytes.Equal(first.Output, tc.output) {
					t.Fatalf("first output = %s, want the adapter output %s", first.Output, tc.output)
				}
			}
			if tc.afterFirst != nil {
				tc.afterFirst(t, harness)
			}
			if state := harness.intentState(t); state != effect.StateSucceeded {
				t.Fatalf("intent state = %s, want succeeded", state)
			}
			if got := harness.observed.count("result_not_recorded"); got != tc.wantUnrec {
				t.Fatalf("observed result_not_recorded %d times, want %d", got, tc.wantUnrec)
			}
			_, err := harness.run(t.Context(), tc.replay())
			if class := classOf(err); class != tc.wantClass {
				t.Fatalf("error = %v, want class %s", err, tc.wantClass)
			}
			if tc.wantSuffix != "" {
				requireDetailSuffix(t, err, tc.wantClass, tc.wantSuffix)
			}
			if harness.calls != tc.wantCalls {
				t.Fatalf("adapter calls = %d, want %d", harness.calls, tc.wantCalls)
			}
			if state := harness.intentState(t); state != effect.StateSucceeded {
				t.Fatalf("intent state after the replay = %s, want succeeded", state)
			}
		})
	}
}

type succeedingProvider struct{}

func (*succeedingProvider) SupportsIdempotency() bool { return false }

func (*succeedingProvider) Invoke(context.Context, *effect.Invocation) (effect.Outcome, error) {
	return effect.Outcome{Succeeded: true, Receipt: json.RawMessage(`{}`)}, nil
}

type passthroughInvocation struct {
	durable.Invocation
}

func (passthroughInvocation) RunAction(ctx context.Context, _ string, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	return fn(ctx)
}

func TestDurableTurnEffectReplayReturnsTheRecordedResult(t *testing.T) {
	harness := newReplayHarness(t, &replayConfig{
		output:      json.RawMessage(`{ "html" : "<b>&</b>", "n" : 1 }`),
		inlineLimit: 64 << 10,
	})
	inTurnScope := func() context.Context {
		return durable.WithTurnScope(t.Context(), durable.NewTurnScope(passthroughInvocation{}))
	}
	first, err := harness.run(inTurnScope(), replayRequest(replayArguments))
	if err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	replay, err := harness.run(inTurnScope(), replayRequest(replayArguments))
	if err != nil {
		t.Fatalf("replay = %v; want %s %s %v %v", err, first.Output, first.Class, first.Truncated, first.Untrusted)
	}
	if !bytes.Equal(first.Output, replay.Output) || first.Class != replay.Class || first.Truncated != replay.Truncated || first.Untrusted != replay.Untrusted {
		t.Fatalf("replay = %s %s %v %v; want %s %s %v %v", replay.Output, replay.Class, replay.Truncated, replay.Untrusted, first.Output, first.Class, first.Truncated, first.Untrusted)
	}
	if harness.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", harness.calls)
	}
}

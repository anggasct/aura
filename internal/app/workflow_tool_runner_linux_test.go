//go:build linux

package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/toolbroker/toolbrokertest"
	"github.com/anggasct/aura/internal/store"
)

type effectIntentIDs struct {
	sessionID      string
	turnID         string
	requestID      string
	idempotencyKey string
}

func writeFileArgs(t *testing.T, name, content string) json.RawMessage {
	t.Helper()
	args, err := json.Marshal(map[string]string{"path": name, "content": content})
	if err != nil {
		t.Fatalf("encode arguments: %v", err)
	}
	return args
}

func effectIntentsOf(t *testing.T, db *sql.DB, key string) []effectIntentIDs {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT session_id, turn_id, tool_call_id, idempotency_key FROM effect_intent WHERE idempotency_key = ?`, key)
	if err != nil {
		t.Fatalf("query effect intents: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var intents []effectIntentIDs
	for rows.Next() {
		var ids effectIntentIDs
		if err := rows.Scan(&ids.sessionID, &ids.turnID, &ids.requestID, &ids.idempotencyKey); err != nil {
			t.Fatalf("scan effect intent: %v", err)
		}
		intents = append(intents, ids)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read effect intents: %v", err)
	}
	return intents
}

func digestOf(runID, stepID string) string {
	sum := sha256.Sum256([]byte(runID + "/" + stepID))
	return hex.EncodeToString(sum[:])[:24]
}

func stepCallAt(t *testing.T, db *sql.DB, runID, stepID string, attempt int) context.Context {
	t.Helper()
	return stepContextFor(t, db, ownerStepCall(runID, stepID, attempt))
}

const probeTool = "probe_effect"

type toolProbe struct {
	mu       sync.Mutex
	requests []*toolbroker.ToolRequest
}

func (p *toolProbe) seen() []*toolbroker.ToolRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

func registerToolProbe(t *testing.T, runner workflow.ToolRunner) *toolProbe {
	t.Helper()
	toolRunner, ok := runner.(*workflowToolRunner)
	if !ok {
		t.Fatalf("runner is %T, want *workflowToolRunner", runner)
	}
	probe := &toolProbe{}
	toolbrokertest.Register(t, toolRunner.executor.Broker(), toolbrokertest.Stub{
		Name: probeTool, Effectful: true,
		Run: func(_ context.Context, req *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
			captured := *req
			probe.mu.Lock()
			probe.requests = append(probe.requests, &captured)
			probe.mu.Unlock()
			return toolbroker.ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		},
	})
	return probe
}

func probeArgs(step string) json.RawMessage {
	return json.RawMessage(`{"step":"` + step + `"}`)
}

func runSessionEvents(t *testing.T, db *sql.DB, runID string) []uint64 {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT sequence FROM runtime_event WHERE session_id = ? AND kind = 'tool.requested' ORDER BY sequence`, workflow.RunSessionID(runID))
	if err != nil {
		t.Fatalf("query run session events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var sequences []uint64
	for rows.Next() {
		var sequence uint64
		if err := rows.Scan(&sequence); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		sequences = append(sequences, sequence)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read events: %v", err)
	}
	return sequences
}

func withKnowledgePersona(cfg *config.Config) {
	cfg.Agents.Definitions = append(cfg.Agents.Definitions, config.AgentDefinition{
		ID: "librarian", Description: "d", Instructions: "i", Knowledge: []string{"docs"},
	})
}

func TestWorkflowToolStepCarriesItsOrigin(t *testing.T) {
	runner, db, workspace := newWorkflowToolRunnerFixtureWith(t, withKnowledgePersona)
	probe := registerToolProbe(t, runner)

	librarianCall := func(runID, stepID string) *workflow.StepCall {
		call := ownerStepCall(runID, stepID, 0)
		call.Origin.AgentID = "librarian"
		return call
	}

	t.Run("the request carries the origin principal, class and agent", func(t *testing.T) {
		before := len(probe.seen())
		if _, err := runner.Invoke(stepContextFor(t, db, librarianCall("run-origin", "step-1")), probeTool, probeArgs("origin")); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		req := probe.seen()[before]
		if req.PrincipalID != principal.OwnerID || req.PrincipalClass != principal.ClassOwner || req.AgentID != "librarian" {
			t.Fatalf("request origin = %q/%q/%q, want %q/%q/librarian", req.PrincipalID, req.PrincipalClass, req.AgentID, principal.OwnerID, principal.ClassOwner)
		}
	})

	t.Run("the taint is the origin's or the approval's", func(t *testing.T) {
		tests := []struct {
			name     string
			origin   bool
			approval bool
			want     bool
		}{
			{name: "neither", want: false},
			{name: "origin only", origin: true, want: true},
			{name: "approval only", approval: true, want: true},
			{name: "both", origin: true, approval: true, want: true},
		}
		for index, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				call := ownerStepCall("run-taint-"+strconv.Itoa(index), "step-1", 0)
				call.Origin.Tainted, call.Approval.Tainted = tc.origin, tc.approval
				before := len(probe.seen())
				if _, err := runner.Invoke(stepContextFor(t, db, call), probeTool, probeArgs("taint"+strconv.Itoa(index))); err != nil {
					t.Fatalf("Invoke: %v", err)
				}
				if got := probe.seen()[before].Tainted; got != tc.want {
					t.Fatalf("request tainted = %v, want %v", got, tc.want)
				}
			})
		}
	})

	t.Run("the trust label follows the approval kind", func(t *testing.T) {
		tests := []struct {
			name string
			kind workflow.ApprovalKind
			want approval.TrustLabel
			code workflow.ErrorCode
		}{
			{name: "installed", kind: workflow.ApprovalInstalled, want: approval.TrustTrustedConfiguration},
			{name: "proposal", kind: workflow.ApprovalProposal, want: approval.TrustDerivedUntrusted},
			{name: "routine", kind: workflow.ApprovalRoutine, want: approval.TrustDerivedUntrusted},
			{name: "empty", kind: "", code: workflow.ErrorCodeApprovalMissing},
			{name: "unknown", kind: "other", code: workflow.ErrorCodeApprovalMissing},
		}
		for index, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				call := ownerStepCall("run-label-"+strconv.Itoa(index), "step-1", 0)
				call.Approval.Kind = tc.kind
				before := len(probe.seen())
				_, err := runner.Invoke(stepContextFor(t, db, call), probeTool, probeArgs("label"+strconv.Itoa(index)))
				if tc.code != "" {
					if code, ok := workflow.CodeOf(err); !ok || code != tc.code {
						t.Fatalf("Invoke error = %v, want %s", err, tc.code)
					}
					if len(probe.seen()) != before {
						t.Fatal("the broker ran for an approval of an unknown kind")
					}
					return
				}
				if err != nil {
					t.Fatalf("Invoke: %v", err)
				}
				if got := probe.seen()[before].Trust; got != tc.want {
					t.Fatalf("request trust = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("the knowledge is the persona's, and a missing persona is refused", func(t *testing.T) {
		before := len(probe.seen())
		if _, err := runner.Invoke(stepContextFor(t, db, librarianCall("run-knowledge", "step-1")), probeTool, probeArgs("knowledge")); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if got := probe.seen()[before].Knowledge; !slices.Equal(got, []string{"docs"}) {
			t.Fatalf("request knowledge = %v, want [docs]", got)
		}
		before = len(probe.seen())
		if _, err := runner.Invoke(stepContextFor(t, db, ownerStepCall("run-knowledge-main", "step-1", 0)), probeTool, probeArgs("knowledge-main")); err != nil {
			t.Fatalf("Invoke as main: %v", err)
		}
		if got := probe.seen()[before].Knowledge; len(got) != 0 {
			t.Fatalf("request knowledge for a persona without any = %v, want none", got)
		}
		ghost := ownerStepCall("run-ghost", "step-1", 0)
		ghost.Origin.AgentID = "ghost"
		before = len(probe.seen())
		_, err := runner.Invoke(stepContextFor(t, db, ghost), probeTool, probeArgs("ghost"))
		if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodePersonaMissing {
			t.Fatalf("Invoke error = %v, want %s", err, workflow.ErrorCodePersonaMissing)
		}
		if len(probe.seen()) != before {
			t.Fatal("the broker ran for a persona that is not configured")
		}
	})

	t.Run("the deadline is the step context's, and a context without one is refused", func(t *testing.T) {
		call := ownerStepCall("run-deadline", "step-1", 0)
		ctx := stepContextFor(t, db, call)
		want, ok := ctx.Deadline()
		if !ok {
			t.Fatal("the test context has no deadline")
		}
		before := len(probe.seen())
		if _, err := runner.Invoke(ctx, probeTool, probeArgs("deadline")); err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if got := probe.seen()[before].Deadline; !got.Equal(want) {
			t.Fatalf("request deadline = %v, want %v", got, want)
		}
		before = len(probe.seen())
		_, err := runner.Invoke(workflow.WithStepCall(t.Context(), call), probeTool, probeArgs("deadline"))
		if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeExecutorInvalid {
			t.Fatalf("Invoke without a deadline error = %v, want %s", err, workflow.ErrorCodeExecutorInvalid)
		}
		if len(probe.seen()) != before {
			t.Fatal("the broker ran for a step without a deadline")
		}
	})

	t.Run("the session is the run's and the ids are per run and step whatever the attempt", func(t *testing.T) {
		before := len(probe.seen())
		for attempt := range 3 {
			if _, err := runner.Invoke(stepCallAt(t, db, "run-ids", "step-a", attempt), probeTool, probeArgs("ids")); err != nil {
				t.Fatalf("Invoke attempt %d: %v", attempt, err)
			}
		}
		if _, err := runner.Invoke(stepCallAt(t, db, "run-ids", "step-b", 0), probeTool, probeArgs("ids-b")); err != nil {
			t.Fatalf("Invoke step-b: %v", err)
		}
		requests := probe.seen()[before:]
		if len(requests) != 2 {
			t.Fatalf("the effectful tool ran %d times, want one per step", len(requests))
		}
		digest := digestOf("run-ids", "step-a")
		first := requests[0]
		if first.SessionID != "wf-run-ids" || first.TurnID != "turn-"+digest || first.RequestID != "req-"+digest || first.IdempotencyKey != "wf/run-ids/step-a" {
			t.Fatalf("step-a request ids = %q/%q/%q/%q", first.SessionID, first.TurnID, first.RequestID, first.IdempotencyKey)
		}
		other := requests[1]
		if other.SessionID != first.SessionID || other.TurnID == first.TurnID || other.RequestID == first.RequestID || other.IdempotencyKey == first.IdempotencyKey {
			t.Fatalf("step-b ids %q/%q/%q/%q do not share the session and differ otherwise", other.SessionID, other.TurnID, other.RequestID, other.IdempotencyKey)
		}
		intents := effectIntentsOf(t, db, "wf/run-ids/step-a")
		if len(intents) != 1 || intents[0].sessionID != "wf-run-ids" || intents[0].turnID != "turn-"+digest {
			t.Fatalf("step-a effect intents = %+v, want one in the run session", intents)
		}
	})

	t.Run("the same run and step send the same ids whatever the attempt, and the effect is performed once", func(t *testing.T) {
		first, err := runner.Invoke(stepCallAt(t, db, "run-1", "step-a", 0), "write_file", writeFileArgs(t, "a.txt", "one"))
		if err != nil {
			t.Fatalf("first Invoke: %v", err)
		}
		retried, err := runner.Invoke(stepCallAt(t, db, "run-1", "step-a", 1), "write_file", writeFileArgs(t, "a.txt", "one"))
		if err != nil {
			t.Fatalf("retry Invoke with the same arguments: %v", err)
		}
		if !bytes.Equal(retried, first) {
			t.Fatalf("retry Invoke output = %s, want the first output %s", retried, first)
		}
		_, err = runner.Invoke(stepCallAt(t, db, "run-1", "step-a", 2), "write_file", writeFileArgs(t, "a.txt", "two"))
		if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultInvalidArgument {
			t.Fatalf("retry Invoke with other arguments error = %v, want %s", err, toolbroker.ResultInvalidArgument)
		}
		content, err := os.ReadFile(filepath.Join(workspace, "a.txt"))
		if err != nil {
			t.Fatalf("read the written file: %v", err)
		}
		if string(content) != "one" {
			t.Fatalf("file content = %q, want the first attempt's %q", content, "one")
		}
		digest := digestOf("run-1", "step-a")
		want := effectIntentIDs{sessionID: "wf-run-1", turnID: "turn-" + digest, requestID: "req-" + digest, idempotencyKey: "wf/run-1/step-a"}
		got := effectIntentsOf(t, db, "wf/run-1/step-a")
		if len(got) != 1 || got[0] != want {
			t.Fatalf("effect intents = %+v, want %+v", got, want)
		}
	})

	t.Run("a call without a step call is refused", func(t *testing.T) {
		_, err := runner.Invoke(t.Context(), "write_file", writeFileArgs(t, "c.txt", "one"))
		if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeExecutorInvalid {
			t.Fatalf("Invoke error = %v, want %s", err, workflow.ErrorCodeExecutorInvalid)
		}
		if _, err := os.Stat(filepath.Join(workspace, "c.txt")); !os.IsNotExist(err) {
			t.Fatalf("the refused call left c.txt behind (err=%v)", err)
		}
	})

	t.Run("empty arguments stay a step failure", func(t *testing.T) {
		contexts := map[string]context.Context{
			"with a step call":    stepCallAt(t, db, "run-1", "step-a", 0),
			"without a step call": t.Context(),
		}
		for name, ctx := range contexts {
			_, err := runner.Invoke(ctx, "write_file", json.RawMessage(`{}`))
			if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeStepFailed {
				t.Fatalf("%s: Invoke error = %v, want %s", name, err, workflow.ErrorCodeStepFailed)
			}
		}
	})
}

func TestWorkflowStepRefusesAMissingRunSession(t *testing.T) {
	runner, db, _ := newWorkflowToolRunnerFixture(t)
	probe := registerToolProbe(t, runner)

	sessionCount := func(t *testing.T, runID string) int {
		t.Helper()
		var count int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session WHERE id = ?`, workflow.RunSessionID(runID)).Scan(&count); err != nil {
			t.Fatalf("count run sessions: %v", err)
		}
		return count
	}
	seed := func(t *testing.T, runID, owner, agent string) context.Context {
		t.Helper()
		tx, err := db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if err := store.CreateSessionTx(t.Context(), tx, &store.Session{ID: workflow.RunSessionID(runID), OwnerID: owner, AgentID: agent}); err != nil {
			t.Fatalf("create run session: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		t.Cleanup(cancel)
		return workflow.WithStepCall(ctx, ownerStepCall(runID, "step-1", 0))
	}

	t.Run("a deleted session is not recreated", func(t *testing.T) {
		ctx := seed(t, "run-deleted", principal.OwnerID, auraagent.DefaultID)
		if _, err := db.ExecContext(t.Context(), `DELETE FROM session WHERE id = ?`, workflow.RunSessionID("run-deleted")); err != nil {
			t.Fatalf("delete run session: %v", err)
		}
		_, err := runner.Invoke(ctx, probeTool, probeArgs("deleted"))
		if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeSessionInvalid {
			t.Fatalf("Invoke error = %v, want %s", err, workflow.ErrorCodeSessionInvalid)
		}
		if n := sessionCount(t, "run-deleted"); n != 0 {
			t.Fatalf("run session rows after the refusal = %d, want 0", n)
		}
	})

	tests := []struct {
		name  string
		owner string
		agent string
	}{
		{name: "another agent", owner: principal.OwnerID, agent: "other"},
		{name: "another owner", owner: "discord:111", agent: auraagent.DefaultID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runID := "run-" + strings.ReplaceAll(tc.name, " ", "-")
			ctx := seed(t, runID, tc.owner, tc.agent)
			_, err := runner.Invoke(ctx, probeTool, probeArgs(runID))
			if code, ok := workflow.CodeOf(err); !ok || code != workflow.ErrorCodeSessionInvalid {
				t.Fatalf("Invoke error = %v, want %s", err, workflow.ErrorCodeSessionInvalid)
			}
		})
	}
	if len(probe.seen()) != 0 {
		t.Fatal("the broker ran in a session that is not the run's")
	}
}

func TestWorkflowStepsShareTheirRunsJournal(t *testing.T) {
	runner, db, _ := newWorkflowToolRunnerFixture(t)
	registerToolProbe(t, runner)

	for _, step := range []string{"step-a", "step-b"} {
		if _, err := runner.Invoke(stepCallAt(t, db, "run-journal", step, 0), probeTool, probeArgs(step)); err != nil {
			t.Fatalf("Invoke %s: %v", step, err)
		}
	}
	if got := runSessionEvents(t, db, "run-journal"); !slices.Equal(got, []uint64{1, 2}) {
		t.Fatalf("tool.requested sequences = %v, want [1 2]", got)
	}
	if _, err := runner.Invoke(stepCallAt(t, db, "run-journal", "step-a", 1), probeTool, probeArgs("step-a")); err != nil {
		t.Fatalf("replay of step-a: %v", err)
	}
	if got := runSessionEvents(t, db, "run-journal"); !slices.Equal(got, []uint64{1, 2}) {
		t.Fatalf("tool.requested sequences after a replay = %v, want [1 2]", got)
	}
}

func TestDelegatedWorkflowStartNoLongerRunsAsOwner(t *testing.T) {
	var cfg config.Config
	runner, db, workspace := newWorkflowToolRunnerFixtureWith(t, func(c *config.Config) {
		c.Agents.Definitions = append(c.Agents.Definitions, config.AgentDefinition{
			ID: "kantor", Description: "d", Instructions: "i", Audience: config.AudienceTrusted,
			Tools: []string{"read_file", "write_file"}, PrincipalTools: []string{"write_file"}, Workflows: []string{"write-note"},
		})
		cfg = *c
	})
	registry, err := buildAgentRegistry(&cfg)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	disk := workflow.NewStore(db)
	fake := durable.NewFake()
	interpreter := workflow.NewInterpreter(disk, fake, &workflow.Options{Tools: runner, Logger: slog.Default()})
	tool := "write_file"
	spec := &workflow.Spec{
		ID: "write-note", Goal: "Write a note", Version: 1, Source: workflow.SourceDefined,
		Steps: []workflow.StepSpec{
			{ID: "approve", Timeout: 5 * time.Second, Executor: workflow.ExecutorSpec{Kind: workflow.KindApproval}},
			{
				ID: "write", DependsOn: []string{"approve"}, Timeout: 5 * time.Second,
				Executor: workflow.ExecutorSpec{Kind: workflow.KindTool, ToolID: &tool, ToolArgs: writeFileArgs(t, "note.txt", "delegated")},
			},
		},
	}
	deps := &workflow.ValidationDeps{
		KnownTools:     toolsbuiltin.DefinitionNames(),
		EffectfulTools: toolsbuiltin.EffectfulToolNames(),
		CheckToolCall:  acceptToolCall,
	}
	if err := interpreter.Install(t.Context(), []*workflow.Spec{spec}, deps); err != nil {
		t.Fatalf("Install: %v", err)
	}
	controller, err := newWorkflowToolController(interpreter, disk, registry, nil)
	if err != nil {
		t.Fatalf("newWorkflowToolController: %v", err)
	}
	state, err := controller.Start(t.Context(), &toolsbuiltin.WorkflowCaller{
		PrincipalID: "discord:111", PrincipalClass: principal.ClassTrusted, AgentID: "kantor", SessionID: "kantor/discord:office/dm:111",
	}, "write-note", "")
	if err != nil {
		t.Fatalf("workflow_start: %v", err)
	}
	var run *workflow.RunSummary
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if run, err = disk.Run(t.Context(), state.RunID); err == nil && run.Status == workflow.RunSuspended {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run == nil || run.Status != workflow.RunSuspended {
		t.Fatalf("delegated run = %+v, %v; want suspended at the approval step", run, err)
	}
	if err := fake.Signal(t.Context(), durable.RunRef{Key: run.DurableKey}, "approval.approve", []byte(`{"decision":"approve"}`)); err != nil {
		t.Fatalf("Signal approve: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if run, err = disk.Run(t.Context(), state.RunID); err == nil && run.Status == workflow.RunFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if run == nil || run.Status != workflow.RunFailed {
		t.Fatalf("delegated run = %+v, %v; want failed", run, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "note.txt")); !os.IsNotExist(err) {
		t.Fatalf("the delegated step wrote note.txt (err=%v)", err)
	}
	call := ownerStepCall(state.RunID, "write", 0)
	call.Origin = run.Origin
	_, err = runner.Invoke(stepContextFor(t, db, call), tool, writeFileArgs(t, "note.txt", "delegated"))
	if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultToolTierDenied {
		t.Fatalf("Invoke as the delegated origin error = %v, want %s", err, toolbroker.ResultToolTierDenied)
	}
}

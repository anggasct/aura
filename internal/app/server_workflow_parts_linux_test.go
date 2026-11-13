package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/kit/access"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimeadk "github.com/anggasct/aura/internal/platform/turn/adk"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"
)

const deskReadsYAML = `id: desk-reads
version: 1
goal: Read the desk note
source: defined
run_as: desk
steps:
  - id: read
    executor: { kind: tool, tool: read_file, args: { path: desk.txt } }
    timeout: 1m
`

const deskPersonaYAML = `agents:
  definitions:
    - id: desk
      description: trusted desk
      instructions: "You answer trusted callers."
      audience: trusted
      tools: [read_file, workflow_start, workflow_status]
      principal_tools: [read_file, workflow_start, workflow_status]
      workflows: [desk-reads]
access:
  budgets:
    trusted:
      max_tokens_per_day: 100000
      max_turns_per_minute: 50
`

func writeDeskServerConfig(t *testing.T) string {
	t.Helper()
	definitions := t.TempDir()
	if err := os.WriteFile(filepath.Join(definitions, "desk-reads.yaml"), []byte(deskReadsYAML), 0o600); err != nil {
		t.Fatalf("write definition: %v", err)
	}
	extra := deskPersonaYAML + fmt.Sprintf("workflows:\n  definitions_dir: %s\n", definitions)
	path := writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, extra)
	workspace := loadCompositionConfig(t, path).Config.Tools.Workspace
	if err := os.WriteFile(filepath.Join(workspace, "desk.txt"), []byte("desk"), 0o600); err != nil {
		t.Fatalf("write desk.txt: %v", err)
	}
	return path
}

func createTestSession(t *testing.T, db *sql.DB, id, owner string) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: id, OwnerID: owner, Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

func executeWorkflowTool(t *testing.T, executor *toolsbuiltin.Executor, tool string, caller *toolsbuiltin.WorkflowCaller, arguments string) json.RawMessage {
	t.Helper()
	output, err := executor.Execute(t.Context(), &runtimeadk.BuiltinToolRequest{
		RequestID: "call-" + tool, TurnID: "turn-" + tool, SessionID: caller.SessionID, PrincipalID: caller.PrincipalID,
		PrincipalClass: caller.PrincipalClass, AgentID: caller.AgentID, ToolName: tool, ToolVersion: "v1",
		Arguments: json.RawMessage(arguments), Trust: string(approval.TrustOwnerInput),
		Deadline: time.Now().Add(time.Minute), IdempotencyKey: "parts/" + tool,
	})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return output
}

func TestServerWorkflowToolsStartARunOnTheDefaultConfiguration(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	path := writeDeskServerConfig(t)
	result := loadCompositionConfig(t, path)
	result.CapabilityReport = capabilitytest.Report(t, "workspace-read")
	srv := &compositionServer{app: buildApp(t, serverCompositionOptions(result, &syncBuffer{}))}
	t.Cleanup(func() {
		if srv.ran != nil {
			srv.cancel()
			<-srv.ran
		}
		_ = srv.app.Close(context.Background())
	})
	srv.run(t)

	db := compositionDatabase(t, path)
	caller := &toolsbuiltin.WorkflowCaller{SessionID: "desk/discord:friend/dm", PrincipalID: "friend", PrincipalClass: principal.ClassTrusted, AgentID: "desk"}
	createTestSession(t, db, caller.SessionID, caller.PrincipalID)

	var started toolsbuiltin.WorkflowRunState
	if err := json.Unmarshal(executeWorkflowTool(t, srv.app.builtin, "workflow_start", caller, `{"id":"desk-reads"}`), &started); err != nil || started.RunID == "" {
		t.Fatalf("workflow_start = %+v, %v", started, err)
	}
	var durableKey string
	if err := db.QueryRowContext(t.Context(), `SELECT durable_key FROM workflow_run WHERE id = ?`, started.RunID).Scan(&durableKey); err != nil {
		t.Fatalf("read the durable key: %v", err)
	}
	if got, want := awaitRunEnded(t, db, durableKey), (storedRun{handler: workflow.HandlerName, state: "succeeded"}); got != want {
		t.Fatalf("workflow run = %+v, want %+v", got, want)
	}
	var status toolsbuiltin.WorkflowRunState
	if err := json.Unmarshal(executeWorkflowTool(t, srv.app.builtin, "workflow_status", caller, `{"run_id":"`+started.RunID+`"}`), &status); err != nil {
		t.Fatal(err)
	}
	if status.Status != workflow.RunSucceeded || len(status.Steps) != 1 || status.Steps[0].ID != "read" || status.Steps[0].Status != workflow.StepSucceeded {
		t.Fatalf("workflow_status = %+v", status)
	}
	srv.stopRun(t)
}

func TestServerPostsWorkflowCompletionsOnTheDefaultConfiguration(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	path := writeCompositionConfig(t, freeLoopbackAddress(t), compositionModelsYAML, "")
	seedConfiguredStorage(t, path)
	db := compositionDatabase(t, path)
	routine := quarantineRoutine("done_routine")
	routine.Steps[0].Executor.ToolArgs = json.RawMessage(`{"path":"note.txt"}`)
	if err := workflow.NewStore(db).SaveDefinition(t.Context(), routine); err != nil {
		t.Fatalf("seed routine: %v", err)
	}
	createTestSession(t, db, "sess-done", principal.OwnerID)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO workflow_run (id, definition_id, definition_version, goal, input_json, status, created_at, updated_at, principal_id, principal_class, agent_id, origin_session_id, tainted) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"run-done", routine.ID, routine.Version, routine.Goal, `{}`, workflow.RunSucceeded, now, now, principal.OwnerID, string(principal.ClassOwner), "main", "sess-done", 0); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	result := loadCompositionConfig(t, path)
	srv := &compositionServer{app: buildApp(t, serverCompositionOptions(result, &syncBuffer{}))}
	t.Cleanup(func() {
		if srv.ran != nil {
			srv.cancel()
			<-srv.ran
		}
		_ = srv.app.Close(context.Background())
	})
	srv.run(t)

	awaitSchedule(t, "the completion of run-done is posted", func() bool {
		return countRows(t, db, `SELECT COUNT(*) FROM runtime_event WHERE session_id = ? AND id = ?`, "sess-done", "workflow-run-done") == 1
	})
	if n := countRows(t, db, `SELECT COUNT(*) FROM workflow_run WHERE id = ? AND completion_notified_at IS NOT NULL`, "run-done"); n != 1 {
		t.Fatalf("notified runs = %d, want 1", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM runtime_event WHERE session_id = ?`, "sess-done"); n != 1 {
		t.Fatalf("events in sess-done = %d, want 1", n)
	}
	srv.stopRun(t)
}

func TestServerStartsScheduledWorkflowsOnTheDefaultConfiguration(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	path := writeWorkflowServerConfig(t)
	result := loadCompositionConfig(t, path)
	result.CapabilityReport = capabilitytest.Report(t, "workspace-read")
	b := newBuild(serverCompositionOptions(result, &syncBuffer{}))
	t.Cleanup(func() {
		if b.group != nil {
			_ = b.group.Stop(context.WithoutCancel(t.Context()))
		}
	})
	for _, step := range b.phases(t.Context()) {
		if err := step(); err != nil {
			t.Fatalf("build phase: %v", err)
		}
	}

	request := &schedule.TurnRequest{PrincipalID: principal.OwnerID, IdempotencyKey: "occ-1", WorkflowID: "two-reads"}
	first, err := b.server.scheduledWorkflows.start(t.Context(), request)
	if err != nil || !first.Succeeded || first.TurnID == "" {
		t.Fatalf("scheduled start = %+v, %v", first, err)
	}
	second, err := b.server.scheduledWorkflows.start(t.Context(), request)
	if err != nil || second.TurnID != first.TurnID {
		t.Fatalf("a repeated occurrence started %+v, %v instead of reusing %s", second, err, first.TurnID)
	}
	if n := countRows(t, b.db, `SELECT COUNT(*) FROM workflow_run WHERE durable_key = ?`, "sched-occ-1"); n != 1 {
		t.Fatalf("workflow runs with the durable key sched-occ-1 = %d, want 1", n)
	}
}

type proposalChannel struct {
	mu      sync.Mutex
	cards   []discord.ProposalCard
	decide  discord.ProposalDecider
	offered chan struct{}
	session string
}

func newProposalChannel(session string) *proposalChannel {
	return &proposalChannel{offered: make(chan struct{}, 4), session: session}
}

func (c *proposalChannel) Start(ctx context.Context, _ runtimeingress.IngressSink) error {
	<-ctx.Done()
	return nil
}

func (c *proposalChannel) Deliver(context.Context, *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	return runtimechannelhost.ProviderReceipt{}, nil
}

func (c *proposalChannel) Health(context.Context) runtimechannelhost.ChannelHealth {
	return runtimechannelhost.ChannelHealth{Status: runtimechannelhost.ChannelHealthy}
}

func (c *proposalChannel) OfferProposal(_ context.Context, card discord.ProposalCard, decide discord.ProposalDecider) error {
	c.mu.Lock()
	c.cards = append(c.cards, card)
	c.decide = decide
	c.mu.Unlock()
	c.offered <- struct{}{}
	return nil
}

func (c *proposalChannel) ReplyTarget(sessionID string) (access.HandoffTarget, bool) {
	return access.HandoffTarget{Channel: principal.ChannelDiscord, Account: "front", ChannelID: "333"}, sessionID == c.session
}

func TestServerOffersAProposalCardOnTheDefaultConfiguration(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	path := writeWorkflowServerConfig(t)
	result := loadCompositionConfig(t, path)
	result.CapabilityReport = capabilitytest.Report(t, "workspace-read")
	b := newBuild(serverCompositionOptions(result, &syncBuffer{}))
	t.Cleanup(func() {
		if b.group != nil {
			_ = b.group.Stop(context.WithoutCancel(t.Context()))
		}
	})
	phases := b.phases(t.Context())
	for _, step := range phases[:len(phases)-1] {
		if err := step(); err != nil {
			t.Fatalf("build phase: %v", err)
		}
	}
	owner := &toolsbuiltin.WorkflowCaller{SessionID: "main/terminal:owner", PrincipalID: principal.OwnerID, PrincipalClass: principal.ClassOwner, AgentID: "main"}
	channel := newProposalChannel(owner.SessionID)
	b.server.channels = append(b.server.channels, channel)
	if err := phases[len(phases)-1](); err != nil {
		t.Fatalf("surface phase: %v", err)
	}
	createTestSession(t, b.db, owner.SessionID, owner.PrincipalID)
	if err := b.group.Start(t.Context()); err != nil {
		t.Fatalf("start the lifecycle: %v", err)
	}

	var proposed toolsbuiltin.WorkflowProposal
	spec := `{"spec":{"goal":"Read the note","steps":[{"id":"read","executor":{"kind":"tool","tool":"read_file","args":{"path":"first.txt"}},"timeout":60000000000}]}}`
	if err := json.Unmarshal(executeWorkflowTool(t, b.builtin, "workflow_propose", owner, spec), &proposed); err != nil || proposed.ProposalID == "" {
		t.Fatalf("workflow_propose = %+v, %v", proposed, err)
	}
	if !strings.Contains(proposed.Plan, "read_file") {
		t.Fatalf("the plan does not show the step's call:\n%s", proposed.Plan)
	}
	select {
	case <-channel.offered:
	case <-time.After(compositionServerWait):
		t.Fatal("no proposal card was offered")
	}
	channel.mu.Lock()
	cards, decide := channel.cards, channel.decide
	channel.mu.Unlock()
	if len(cards) != 1 || cards[0].ID != proposed.ProposalID || cards[0].Plan != proposed.Plan {
		t.Fatalf("cards = %+v, want one for %s with the proposed plan", cards, proposed.ProposalID)
	}
	reply, err := decide(t.Context(), true)
	if err != nil || !strings.HasPrefix(reply, "approved "+proposed.ProposalID+": run ") {
		t.Fatalf("approve from the card = %q, %v", reply, err)
	}
	runID := strings.TrimPrefix(reply, "approved "+proposed.ProposalID+": run ")
	if n := countRows(t, b.db, `SELECT COUNT(*) FROM workflow_run WHERE id = ? AND principal_id = ? AND origin_session_id = ?`, runID, owner.PrincipalID, owner.SessionID); n != 1 {
		t.Fatalf("workflow runs %s with the proposer as origin = %d, want 1", runID, n)
	}
}

func TestServerWorkflowPartsNeedTheirDependencies(t *testing.T) {
	for name, strip := range map[string]func(b *build){
		"no interpreter":   func(b *build) { b.interpreter = nil },
		"no surface tools": func(b *build) { b.builtin = nil },
		"no tool broker":   func(b *build) { b.builtin = &toolsbuiltin.Executor{} },
	} {
		t.Run(name, func(t *testing.T) {
			b := buildServerPhases(t, "")
			strip(b)
			before := b.group.Names()
			err := b.wireWorkflowParts()
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("wireWorkflowParts = %v, want %s", err, ErrorCodeInvalidArgument)
			}
			if after := b.group.Names(); len(after) != len(before) {
				t.Fatalf("lifecycle entries %v changed to %v", before, after)
			}
		})
	}
}

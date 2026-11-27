package toolbroker

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	durablesqlite "github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

type operationStub struct {
	name      string
	risk      tools.Risk
	fresh     bool
	notice    bool
	sink      bool
	previewed bool

	capabilities []string
}

func (s operationStub) definition() tools.Definition {
	definition := tools.Definition{
		Name: s.name, Version: tools.Version, Risk: s.risk, Operation: "stub." + s.name,
		FreshApproval: s.fresh, OwnerNotice: s.notice, ExternalSink: s.sink,
		Validator: func(raw json.RawMessage) (json.RawMessage, error) {
			value, err := tools.DecodeObject(raw)
			if err != nil {
				return nil, err
			}
			if err := tools.RejectUnknown(value, "target"); err != nil {
				return nil, err
			}
			if err := tools.ValidateString(value, "target"); err != nil {
				return nil, err
			}
			return json.Marshal(value)
		},
	}
	_ = definition.ApplyOperationFlags()
	definition.RequiredCapabilities = slices.Clone(s.capabilities)
	return definition
}

type stubOperations []ToolRegistration

func (p stubOperations) Tools() []ToolRegistration { return slices.Clone(p) }

type operationLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *operationLog) add(entry string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entry)
}

func (l *operationLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries)
}

type operationOptions struct {
	reachable bool
	answer    deciderAnswer
	preview   Previewer
	observer  effect.Observer
	secrets   []string
	access    AccessPolicy
}

type operationRig struct {
	broker    *Broker
	executor  *effect.Executor
	decider   *scriptedDecider
	observed  *observedApprovals
	log       *operationLog
	reachable atomic.Bool
	handled   atomic.Int32
	previewed atomic.Int32
	preview   Previewer
}

func seedSession(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC()
	if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{ID: "session-1", OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create session: %v", err)
	}
}

func openOperationDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), durablesqlite.Schema(), effect.Schema(), principal.Schema(), storage.Schema())
	seedSession(t, db)
	return db
}

func newOperationRig(t *testing.T, db *sql.DB, options *operationOptions, stubs ...operationStub) *operationRig {
	t.Helper()
	rig := &operationRig{observed: &observedApprovals{}, log: &operationLog{}}
	rig.reachable.Store(options.reachable)
	rig.preview = options.preview
	if rig.preview == nil {
		rig.preview = defaultPreview
	}
	rig.decider = &scriptedDecider{answer: options.answer}
	if rig.decider.answer == nil {
		rig.decider.answer = approveEvery
	}
	journal, err := effect.NewJournal(db, effect.Options{Logger: slog.New(slog.DiscardHandler), Observer: options.observer, Turns: turnstore.NewTurnStore(db)})
	if err != nil {
		t.Fatalf("new journal: %v", err)
	}
	executor, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	rig.executor = executor
	broker, err := New(&Options{
		HostCapabilities: capabilitytest.Host(t), Effects: executor, Observer: rig.observed.observe,
		Secrets: options.secrets, Access: options.access,
		ApprovalDecider: func(ctx context.Context, prompt *ApprovalPrompt) (bool, error) {
			rig.log.add("decide")
			return rig.decider.decide(ctx, prompt)
		},
		ApprovalReachable: func(string) bool { return rig.reachable.Load() },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rig.broker = broker
	registrations := make(stubOperations, 0, len(stubs))
	for _, stub := range stubs {
		registrations = append(registrations, rig.registration(stub))
	}
	if err := broker.RegisterProvider(registrations); err != nil {
		t.Fatalf("register the stub operations: %v", err)
	}
	return rig
}

func (r *operationRig) registration(stub operationStub) ToolRegistration {
	definition := stub.definition()
	registration := ToolRegistration{Definition: definition, Adapter: r.handle, Rule: DefaultRule(&definition)}
	if stub.previewed || needsPreview(&definition) {
		registration.Preview = r.previewer
	}
	return registration
}

func (r *operationRig) previewer(ctx context.Context, request *ToolRequest) (Preview, error) {
	r.previewed.Add(1)
	r.log.add("preview")
	return r.preview(ctx, request)
}

func (r *operationRig) handle(_ context.Context, _ *ToolRequest, _ approval.Constraints) (ToolResult, error) {
	r.handled.Add(1)
	r.log.add("handle")
	return ToolResult{Output: json.RawMessage(`{"done":true}`)}, nil
}

func defaultPreview(_ context.Context, request *ToolRequest) (Preview, error) {
	var arguments struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
		return Preview{}, err
	}
	return Preview{Lines: []string{"target: " + arguments.Target, "cannot be undone"}, Digest: "set-of-" + arguments.Target}, nil
}

func operationRequest(name, target string) *ToolRequest {
	return &ToolRequest{
		RequestID: "call-1", TurnID: "turn-1", SessionID: "session-1",
		PrincipalID: "owner-1", PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
		ToolName: name, ToolVersion: tools.Version, Arguments: json.RawMessage(`{"target":"` + target + `"}`),
		Trust: approval.TrustOwnerInput, IdempotencyKey: "idempotency-" + name,
		AllocateEventSequence: true, EventInvocation: "invocation-1", EventBranch: "main", EventAuthor: "test",
	}
}

func (r *operationRig) execute(t *testing.T, request *ToolRequest) callOutcome {
	t.Helper()
	return outcomeOf(r.broker.Execute(t.Context(), request))
}

func (r *operationRig) requireUntouched(t *testing.T) {
	t.Helper()
	if prompts := r.decider.asked(); len(prompts) != 0 {
		t.Errorf("approval prompts = %d, want none", len(prompts))
	}
	if previews := r.previewed.Load(); previews != 0 {
		t.Errorf("previewer runs = %d, want none", previews)
	}
	if handled := r.handled.Load(); handled != 0 {
		t.Errorf("handler runs = %d, want none", handled)
	}
}

func (r *operationRig) intentProviders(t *testing.T) []string {
	t.Helper()
	var providers []string
	for _, state := range []effect.State{effect.StateSucceeded, effect.StateStarted, effect.StateUnknown, effect.StateFailed, effect.StatePrepared} {
		intents, err := r.executor.Journal().ListByState(t.Context(), state, 0)
		if err != nil {
			t.Fatalf("list %s intents: %v", state, err)
		}
		for i := range intents {
			providers = append(providers, intents[i].Provider)
		}
	}
	slices.Sort(providers)
	return providers
}

func joinedKeys(rows []actionRow) string {
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, row.key)
	}
	return strings.Join(keys, ",")
}

var (
	previewedStub   = operationStub{name: "aura_stub_previewed", risk: tools.RiskChange, previewed: true}
	readStub        = operationStub{name: "aura_stub_read", risk: tools.RiskRead}
	changeStub      = operationStub{name: "aura_stub_change", risk: tools.RiskChange}
	noticeStub      = operationStub{name: "aura_stub_notice", risk: tools.RiskChange, notice: true}
	freshStub       = operationStub{name: "aura_stub_fresh", risk: tools.RiskChange, fresh: true}
	trustGrantStub  = operationStub{name: "aura_stub_grant", risk: tools.RiskTrustGrant}
	destructiveStub = operationStub{name: "aura_stub_delete", risk: tools.RiskDestructive}
	allStubs        = []operationStub{readStub, changeStub, noticeStub, freshStub, trustGrantStub, destructiveStub}
)

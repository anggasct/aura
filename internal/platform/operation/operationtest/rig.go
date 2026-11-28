package operationtest

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	durablesqlite "github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

var Epoch = time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)

const (
	SessionID = "session-1"
	OwnerID   = "owner-1"
)

type Options struct {
	DB           *sql.DB
	Clock        *durabletest.ManualClock
	Catalogs     []operation.Catalog
	Bindings     func(stubs *Stubs) []operation.Binding
	Tools        []toolbroker.ToolRegistration
	NoApprover   bool
	Access       toolbroker.AccessPolicy
	Observer     effect.Observer
	ToolObserver toolbroker.Observer
	Logger       *slog.Logger
	Telemetry    *telemetry.OperationRecorder
	AnswerCards  func(ctx context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error)

	AllowInvalid bool

	WithDiscovery bool
	Behaviors     []operation.Behavior

	WithoutRegistry bool
}

type Rig struct {
	DB       *sql.DB
	Clock    *durabletest.ManualClock
	Executor *effect.Executor
	Broker   *toolbroker.Broker
	Registry *operation.Registry
	Stubs    *Stubs

	Discovery *operation.Discovery

	RegistryErr error

	mu       sync.Mutex
	prompts  []toolbroker.ApprovalPrompt
	approver bool
}

func OpenDatabase(tb testing.TB, extra ...sqlite.SchemaOwner) *sql.DB {
	tb.Helper()
	schemas := append([]sqlite.SchemaOwner{turnstore.Schema(), durablesqlite.Schema(), effect.Schema(), principal.Schema(), storage.Schema(), operation.Schema()}, extra...)
	db := sqlitetest.OpenMigrated(tb, schemas...)
	now := time.Now().UTC()
	if err := turnstore.NewSessionService(db).Create(tb.Context(), &turn.Session{ID: SessionID, OwnerID: OwnerID, CreatedAt: now, UpdatedAt: now}); err != nil {
		tb.Fatalf("create the session: %v", err)
	}
	CreateLedger(tb, db)
	return db
}

func CreateLedger(tb testing.TB, db *sql.DB) {
	tb.Helper()
	if _, err := db.ExecContext(tb.Context(), ledgerSQL); err != nil {
		tb.Fatalf("create the stub ledger: %v", err)
	}
}

func NewRig(tb testing.TB, options *Options) *Rig {
	tb.Helper()
	if options == nil {
		options = &Options{}
	}
	rig := &Rig{DB: options.DB, Clock: options.Clock, approver: !options.NoApprover}
	if rig.DB == nil {
		rig.DB = OpenDatabase(tb)
	}
	if rig.Clock == nil {
		rig.Clock = durabletest.NewManualClock(Epoch)
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	journal, err := effect.NewJournal(rig.DB, effect.Options{Clock: rig.Clock, Logger: logger, Observer: options.Observer, Turns: turnstore.NewTurnStore(rig.DB)})
	if err != nil {
		tb.Fatalf("new journal: %v", err)
	}
	if rig.Executor, err = effect.NewExecutor(journal); err != nil {
		tb.Fatalf("new executor: %v", err)
	}
	answer := options.AnswerCards
	rig.Broker, err = toolbroker.New(&toolbroker.Options{
		HostCapabilities: capabilitytest.Host(tb),
		Effects:          rig.Executor,
		Clock:            rig.Clock,
		Access:           options.Access,
		Observer:         options.ToolObserver,
		ApprovalDecider: func(ctx context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
			rig.mu.Lock()
			rig.prompts = append(rig.prompts, *prompt)
			rig.mu.Unlock()
			if answer != nil {
				return answer(ctx, prompt)
			}
			return true, nil
		},
		ApprovalReachable: func(string) bool { return rig.approver },
	})
	if err != nil {
		tb.Fatalf("new broker: %v", err)
	}
	if len(options.Tools) > 0 {
		if err := rig.Broker.RegisterProvider(toolList(options.Tools)); err != nil {
			tb.Fatalf("register the pre-existing tools: %v", err)
		}
	}
	rig.Stubs = NewStubs(rig.DB)
	if options.WithoutRegistry {
		return rig
	}
	catalogs := options.Catalogs
	if catalogs == nil {
		catalogs = []operation.Catalog{Catalog()}
	}
	bindings := rig.Stubs.Bindings()
	if options.Bindings != nil {
		bindings = options.Bindings(rig.Stubs)
	}
	rig.Registry, err = operation.NewRegistry(catalogs, bindings, &operation.Deps{
		DB: rig.DB, Effects: rig.Executor, Tools: rig.Broker, Clock: rig.Clock, Logger: logger, Telemetry: options.Telemetry,
	})
	if err != nil && options.AllowInvalid {
		rig.RegistryErr = err
		return rig
	}
	if err != nil {
		tb.Fatalf("new registry: %v", err)
	}
	if err := rig.Broker.RegisterProvider(rig.Registry); err != nil {
		tb.Fatalf("register the operation tools: %v", err)
	}
	if options.WithDiscovery {
		rig.Discovery, err = operation.NewDiscovery(&operation.DiscoveryDeps{Registry: rig.Registry, Behaviors: options.Behaviors, Capabilities: capabilitytest.Host(tb)})
		if err != nil {
			tb.Fatalf("new discovery: %v", err)
		}
		if err := rig.Broker.RegisterProvider(rig.Discovery); err != nil {
			tb.Fatalf("register the discovery tool: %v", err)
		}
	}
	return rig
}

type toolList []toolbroker.ToolRegistration

func (l toolList) Tools() []toolbroker.ToolRegistration { return l }

func (r *Rig) Prompts() []toolbroker.ApprovalPrompt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]toolbroker.ApprovalPrompt(nil), r.prompts...)
}

type Outcome struct {
	Class  toolbroker.ResultClass
	Output string
	Err    error
}

func OutcomeOf(result *toolbroker.ToolResult, err error) Outcome {
	if err != nil {
		class, _ := faultClass(err)
		return Outcome{Class: class, Err: err}
	}
	return Outcome{Class: result.Class, Output: string(result.Output)}
}

func (r *Rig) Execute(ctx context.Context, request *toolbroker.ToolRequest) Outcome {
	result, err := r.Broker.Execute(ctx, request)
	return OutcomeOf(&result, err)
}

func Request(tb testing.TB, tool, subject string) *toolbroker.ToolRequest {
	tb.Helper()
	arguments, err := json.Marshal(map[string]string{"subject": subject})
	if err != nil {
		tb.Fatalf("encode the arguments: %v", err)
	}
	return RequestFor(tool, arguments, "idempotency-"+tool+"-"+subject)
}

func RequestFor(tool string, arguments json.RawMessage, key string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{
		RequestID: "call-1", TurnID: "turn-1", SessionID: SessionID,
		PrincipalID: OwnerID, PrincipalClass: principal.ClassOwner, Origin: turn.OriginTerminal,
		ToolName: tool, ToolVersion: tools.Version, Arguments: arguments,
		Trust: approval.TrustOwnerInput, IdempotencyKey: key,
		AllocateEventSequence: true, EventInvocation: "invocation-1", EventBranch: "main", EventAuthor: "test",
	}
}

func LegacyTools(definitions ...*tools.Definition) []toolbroker.ToolRegistration {
	registrations := make([]toolbroker.ToolRegistration, 0, len(definitions))
	for _, definition := range definitions {
		registrations = append(registrations, toolbroker.ToolRegistration{
			Definition: *definition,
			Adapter: func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
				return toolbroker.ToolResult{Output: json.RawMessage(`{"legacy":true}`)}, nil
			},
			Rule: toolbroker.DefaultRule(definition),
		})
	}
	return registrations
}

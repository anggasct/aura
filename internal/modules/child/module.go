package child

import (
	"context"
	"database/sql"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
)

type ModuleDeps struct {
	DB        *sql.DB
	Sessions  SessionStore
	Turns     TurnRunner
	Runtime   durable.Runtime
	Ledger    BudgetLedger
	Signaler  Signaler
	ModelName string
}

type Module struct {
	store    *SQLiteStore
	registry *StoreRegistry
	handler  *Handler
	ledger   BudgetLedger
}

func New(deps *ModuleDeps) (*Module, error) {
	if deps == nil {
		return nil, errNilArgument("deps")
	}
	if deps.DB == nil {
		return nil, errNilArgument("db")
	}
	if deps.Sessions == nil {
		return nil, errNilArgument("sessions")
	}
	store, err := NewSQLiteStore(deps.DB)
	if err != nil {
		return nil, err
	}
	registry, err := NewStoreRegistry(store, deps.Sessions)
	if err != nil {
		return nil, err
	}
	runs, err := NewStoreHandlerRuns(store)
	if err != nil {
		return nil, err
	}
	runner, err := NewSessionRunner(&SessionRunnerDeps{
		Store:     store,
		Sessions:  deps.Sessions,
		Turns:     deps.Turns,
		ModelName: deps.ModelName,
	})
	if err != nil {
		return nil, err
	}
	ledger := deps.Ledger
	if ledger == nil {
		ledger = NewLedger(nil, 0)
	}
	signaler := deps.Signaler
	if signaler == nil && deps.Runtime != nil {
		signaler, err = NewRuntimeSignaler(deps.Runtime)
		if err != nil {
			return nil, err
		}
	}
	handler, err := NewHandler(&HandlerDeps{
		Runs:     runs,
		Exec:     runner,
		Ledger:   ledger,
		Signaler: signaler,
	})
	if err != nil {
		return nil, err
	}
	return &Module{
		store:    store,
		registry: registry,
		handler:  handler,
		ledger:   ledger,
	}, nil
}

func (m *Module) Store() *SQLiteStore {
	if m == nil {
		return nil
	}
	return m.store
}

func (m *Module) Registry() *StoreRegistry {
	if m == nil {
		return nil
	}
	return m.registry
}

func (m *Module) Handler() *Handler {
	if m == nil {
		return nil
	}
	return m.handler
}

func (m *Module) Ledger() BudgetLedger {
	if m == nil {
		return nil
	}
	return m.ledger
}

func (m *Module) Handle(ctx context.Context, inv durable.Invocation) error {
	if m == nil || m.handler == nil {
		return errNilArgument("module")
	}
	return m.handler.HandleDurable(ctx, inv, time.Now)
}

func (m *Module) Register(registrar durable.HandlerRegistrar) error {
	if m == nil {
		return errNilArgument("module")
	}
	if registrar == nil {
		return errNilArgument("registrar")
	}
	registrar.RegisterHandler(HandlerName, m.Handle)
	return nil
}

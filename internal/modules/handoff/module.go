package handoff

import (
	"database/sql"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type ModuleDeps struct {
	DB        *sql.DB
	Events    EventLister
	Notifiers []Notifier
	Timeout   time.Duration
	Now       func() time.Time
}

type Module struct {
	store   *SQLiteStore
	replies *SQLiteReplyStore
	gate    *Gate
	service *Service
}

func New(deps *ModuleDeps) (*Module, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "handoff module dependencies must not be nil")
	}
	store, err := NewSQLiteStore(deps.DB)
	if err != nil {
		return nil, err
	}
	events := deps.Events
	if events == nil {
		events = turnstore.NewTurnStore(deps.DB)
	}
	replies, err := NewReplyStore(events)
	if err != nil {
		return nil, err
	}
	gate, err := NewGate(store, deps.Timeout, deps.Now)
	if err != nil {
		return nil, err
	}
	service, err := NewService(store, deps.Notifiers, deps.Now)
	if err != nil {
		return nil, err
	}
	return &Module{
		store:   store,
		replies: replies,
		gate:    gate,
		service: service,
	}, nil
}

func (m *Module) Tools() []toolbroker.ToolRegistration {
	if m == nil || m.service == nil {
		return nil
	}
	return []toolbroker.ToolRegistration{ToolRegistration(m.service)}
}

func (m *Module) Store() Store {
	if m == nil {
		return nil
	}
	return m.store
}

func (m *Module) Replies() ReplyStore {
	if m == nil {
		return nil
	}
	return m.replies
}

func (m *Module) Gate() *Gate {
	if m == nil {
		return nil
	}
	return m.gate
}

func (m *Module) Service() *Service {
	if m == nil {
		return nil
	}
	return m.service
}

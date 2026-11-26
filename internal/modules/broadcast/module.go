package broadcast

import (
	"context"
	"database/sql"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
)

type ModuleDeps struct {
	Config   Config
	DB       *sql.DB
	Senders  map[string]Sender
	Runtime  durable.Runtime
	Logger   *slog.Logger
	Observer Observer
}

type Module struct {
	store       *SQLiteStore
	runner      *Runner
	broadcaster *Broadcaster
	runtime     durable.Runtime
}

type mapChannelRegistry struct {
	registered map[string]bool
}

func (r *mapChannelRegistry) Registered(source string) bool {
	return r.registered[source]
}

func New(deps *ModuleDeps) (*Module, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "broadcast module deps must not be nil")
	}
	if deps.DB == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "broadcast database must not be nil")
	}
	if deps.Runtime == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "broadcast durable runtime must not be nil")
	}
	if strings.TrimSpace(deps.Config.Timezone) == "" {
		return nil, fault.New(ErrorCodeInvalidArgument, "broadcast timezone must not be empty")
	}
	location, err := time.LoadLocation(deps.Config.Timezone)
	if err != nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "broadcast timezone is not a valid IANA name")
	}
	startMin, err := ParseHourMinute(deps.Config.QuietHours.Start)
	if err != nil {
		return nil, err
	}
	endMin, err := ParseHourMinute(deps.Config.QuietHours.End)
	if err != nil {
		return nil, err
	}
	store, err := NewSQLiteStore(deps.DB)
	if err != nil {
		return nil, err
	}
	senders := maps.Clone(deps.Senders)
	registered := make(map[string]bool, len(senders))
	for source, sender := range senders {
		if sender != nil {
			registered[source] = true
		}
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	runner, err := NewRunner(
		store,
		deps.Config.Destinations,
		senders,
		RunPolicy{
			MaxAttempts:    deps.Config.MaxAttempts,
			MaxDeliveryAge: time.Duration(deps.Config.MaxDeliveryAge),
			DispatchGap:    time.Duration(deps.Config.MinDispatchGap),
			Fallback:       deps.Config.Fallback,
			MaxDigestItems: deps.Config.MaxDigestItems,
			MaxDigestBytes: deps.Config.MaxDigestBytes,
		},
		logger,
		deps.Observer,
	)
	if err != nil {
		return nil, err
	}
	rt := deps.Runtime
	broadcaster, err := NewBroadcaster(
		Policy{
			Destinations: deps.Config.Destinations,
			Quiet: QuietConfig{
				Enabled:  deps.Config.QuietHours.Enabled,
				Location: location,
				StartMin: startMin,
				EndMin:   endMin,
			},
			MaxDigestItems: deps.Config.MaxDigestItems,
			MaxDigestBytes: deps.Config.MaxDigestBytes,
		},
		&mapChannelRegistry{registered: registered},
		store,
		func(ctx context.Context, itemID string) error {
			return StartItemRun(ctx, rt, itemID)
		},
		deps.Observer,
	)
	if err != nil {
		return nil, err
	}
	return &Module{
		store:       store,
		runner:      runner,
		broadcaster: broadcaster,
		runtime:     rt,
	}, nil
}

func (m *Module) Store() *SQLiteStore {
	return m.store
}

func (m *Module) Runner() *Runner {
	return m.runner
}

func (m *Module) Broadcaster() *Broadcaster {
	return m.broadcaster
}

func (m *Module) Handle(ctx context.Context, inv durable.Invocation) error {
	return m.runner.Handle(ctx, inv)
}

func (m *Module) Resume(ctx context.Context) (int, error) {
	return ResumeActive(ctx, m.runtime, m.store)
}

package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"log/slog"
	"os/user"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/channel/terminal"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/model"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

// terminalBroker denies every tool call. The plain console never presents an
// interactive approval and has no tool grants wired in step 1, so tool
// invocation fails closed rather than executing unconfined.
type terminalBroker struct{}

func (terminalBroker) Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error) {
	return approval.PolicyDecision{Outcome: approval.OutcomeDeny, ReasonCode: "plain_console_denies_tools"}, nil
}

// terminalRunner adapts the runtime engine to the console-facing Runner port.
// It maps the terminal request verbatim onto the runtime TurnRequest and the
// durable runtime events onto the console event view.
type terminalRunner struct {
	engine runtime.AgentRuntime
}

func (r *terminalRunner) Run(ctx context.Context, req *terminal.Request) iter.Seq2[terminal.Event, error] {
	return func(yield func(terminal.Event, error) bool) {
		if req == nil {
			yield(terminal.Event{}, errors.New("terminal: request must not be nil"))
			return
		}
		parts := make([]runtime.InputPart, len(req.Parts))
		for i := range req.Parts {
			parts[i] = runtime.InputPart{Text: req.Parts[i].Text}
		}
		runtimeReq := &runtime.TurnRequest{
			SessionID:      req.SessionID,
			PrincipalID:    req.PrincipalID,
			Origin:         runtime.OriginTerminal,
			Parts:          parts,
			IdempotencyKey: req.IdempotencyKey,
		}
		for ev, err := range r.engine.Run(ctx, runtimeReq) {
			if err != nil {
				yield(terminal.Event{}, err)
				return
			}
			if !yield(terminal.Event{
				Kind:    ev.Kind,
				Author:  ev.Author,
				TurnID:  ev.TurnID,
				Payload: ev.Payload,
			}, nil) {
				return
			}
		}
	}
}

// terminalSessions adapts the store session service to the console Sessions
// port.
type terminalSessions struct {
	sessions store.SessionService
}

var _ terminal.Sessions = (*terminalSessions)(nil)

func (s *terminalSessions) Create(ctx context.Context, owner string) (terminal.Session, error) {
	internal := &store.Session{
		ID:        newTerminalSessionID(),
		OwnerID:   owner,
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := s.sessions.Create(ctx, internal); err != nil {
		return terminal.Session{}, err
	}
	return terminal.Session{ID: internal.ID, OwnerID: internal.OwnerID}, nil
}

func (s *terminalSessions) Get(ctx context.Context, id string) (terminal.Session, error) {
	internal, err := s.sessions.Get(ctx, id)
	if err != nil {
		return terminal.Session{}, err
	}
	return terminal.Session{ID: internal.ID, OwnerID: internal.OwnerID}, nil
}

func (s *terminalSessions) ListEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]terminal.Event, error) {
	events, err := s.sessions.ListEvents(ctx, sessionID, afterSequence, limit)
	if err != nil {
		return nil, err
	}
	out := make([]terminal.Event, len(events))
	for i := range events {
		out[i] = terminal.Event{
			Kind:    events[i].Kind,
			Author:  events[i].Author,
			TurnID:  events[i].TurnID,
			Payload: events[i].Payload,
		}
	}
	return out, nil
}

// runChat is the wire for `aura chat`. It loads config, opens storage, builds
// the runtime engine, and drives the terminal console over stdin/stdout.
func runChat(ctx context.Context, cfg *config.Config, logger *slog.Logger, in io.Reader, out, diag io.Writer) error {
	if _, err := model.BuildRouter(logger, cfg.Models); err != nil {
		return err
	}
	if err := model.RegisterAdapters(logger, cfg.Models); err != nil {
		return err
	}
	db, err := openStorage(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	sessions := store.NewSessionService(db)
	events := store.NewEventStore(db)
	executor, err := runtime.NewADKExecutor(
		"aura", cfg.Models.Definitions["primary"].Model, sessions, events, terminalBroker{}, nil, logger,
	)
	if err != nil {
		return err
	}
	engine, err := runtime.NewEngine(runtime.Config{
		MaxActiveTurns:  cfg.Runtime.MaxActiveTurns,
		MaxPendingTurns: cfg.Runtime.MaxPendingTurns,
		TurnTimeout:     time.Duration(cfg.Runtime.TurnTimeout),
		ShutdownTimeout: time.Duration(cfg.Runtime.ShutdownTimeout),
	}, events, store.NewDedupeStore(db), executor, logger)
	if err != nil {
		return err
	}
	principal, err := localPrincipal()
	if err != nil {
		return err
	}
	console := terminal.NewConsole(
		&terminalRunner{engine: engine},
		&terminalSessions{sessions: sessions},
		terminal.PlainRenderer{},
		in, out, diag,
		terminal.Config{
			MaxInputBytes:       cfg.Terminal.MaxInputBytes,
			InMemoryHistory:     cfg.Terminal.InMemoryHistory,
			SecondInterruptTime: time.Duration(cfg.Terminal.SecondInterruptTime),
		},
		principal,
	)
	return console.Run(ctx)
}

func newTerminalSessionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "sess_" + hex.EncodeToString([]byte("0123456789abcdef01234567"))
	}
	return "sess_" + hex.EncodeToString(b[:])
}

func localPrincipal() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

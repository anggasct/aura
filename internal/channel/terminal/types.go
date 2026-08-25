package terminal

import (
	"context"
	"encoding/json"
	"iter"
	"time"
)

// Input is one normalized prompt the console submits as a turn.
type Input struct {
	Text string
}

// Event is a normalized runtime event as seen by the console. The composition
// root adapts the runtime's durable events onto this view; the console never
// touches model, tool, or store types directly.
type Event struct {
	Kind    string
	Author  string
	TurnID  string
	Payload json.RawMessage
}

// Renderer folds a turn's events into output tailored to a presentation mode.
// It reports the completed assistant text for the primary stream, diagnostics
// for the secondary stream, and whether the turn reached a terminal state.
// The plain renderer writes completed assistant text only; a TTY renderer
// adds streaming presentation on top of the same event view.
type Renderer interface {
	RenderTurn(stream []Event) (assistant string, diagnostics []string, terminal bool)
}

// Session is the console-facing view of a durable conversation.
type Session struct {
	ID      string
	OwnerID string
}

// Runner is the turn boundary the console drives. The engine is the sole
// production implementation; tests substitute a fake.
type Runner interface {
	// Run submits one prompt as a turn and streams its events until the
	// turn reaches a durable terminal state.
	Run(ctx context.Context, req *Request) iter.Seq2[Event, error]
}

// Request is a normalized turn request. SessionID, PrincipalID, Origin, and
// IdempotencyKey mirror the runtime turn contract; the adapter maps them
// verbatim onto the runtime types.
type Request struct {
	SessionID      string
	PrincipalID    string
	Origin         string
	Parts          []Input
	IdempotencyKey string
}

// Sessions is the session lifecycle the console needs for /new and /session.
type Sessions interface {
	Create(ctx context.Context, owner string) (Session, error)
	Get(ctx context.Context, id string) (Session, error)
	ListEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]Event, error)
}

// Config shapes plain console behavior.
type Config struct {
	MaxInputBytes       int
	InMemoryHistory     int
	SecondInterruptTime time.Duration
}

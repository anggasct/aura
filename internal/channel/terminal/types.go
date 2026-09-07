package terminal

import (
	"context"
	"encoding/json"
	"iter"
	"time"
)

type Input struct {
	Text string
}

type Event struct {
	Kind    string
	Author  string
	TurnID  string
	Payload json.RawMessage
}

type Renderer interface {
	RenderTurn(stream []Event) (assistant string, diagnostics []string, terminal bool)
}

type Session struct {
	ID      string
	OwnerID string
}

type Runner interface {
	Run(ctx context.Context, req *Request) iter.Seq2[Event, error]
}

type Request struct {
	SessionID      string
	PrincipalID    string
	Origin         string
	Parts          []Input
	IdempotencyKey string
}

type Sessions interface {
	Create(ctx context.Context, owner string) (Session, error)
	Get(ctx context.Context, id string) (Session, error)
	ListEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]Event, error)
}

type Config struct {
	MaxInputBytes       int
	InMemoryHistory     int
	SecondInterruptTime time.Duration
}

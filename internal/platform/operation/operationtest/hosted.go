package operationtest

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	durablesqlite "github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/toolcall"
)

const (
	hostedHandler = "operation-turn"
	hostedWait    = 20 * time.Second
)

type Body func(ctx context.Context, inv durable.Invocation) error

type Hosted struct {
	DB *sql.DB

	rt       *durablesqlite.Runtime
	mu       sync.Mutex
	runs     int
	bodies   map[string]Body
	returned map[string]int
}

func StartHosted(tb testing.TB, db *sql.DB) *Hosted {
	tb.Helper()
	hosted := &Hosted{DB: db, bodies: map[string]Body{}, returned: map[string]int{}}
	hosted.rt = hosted.AddHost(tb)
	return hosted
}

func (h *Hosted) AddHost(tb testing.TB) *durablesqlite.Runtime {
	tb.Helper()
	rt, err := durablesqlite.New(&durablesqlite.Options{DB: h.DB, Logger: slog.New(slog.DiscardHandler), PollInterval: 10 * time.Millisecond, HostTTL: time.Second, StopGrace: 5 * time.Second})
	if err != nil {
		tb.Fatalf("new durable runtime: %v", err)
	}
	rt.RegisterHandler(hostedHandler, h.handle)
	ctx, stop := context.WithCancel(context.WithoutCancel(tb.Context()))
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	halt := sync.OnceValue(func() bool {
		stop()
		select {
		case <-done:
			return true
		case <-time.After(hostedWait):
			return false
		}
	})
	tb.Cleanup(func() {
		if !halt() {
			tb.Error("the durable host did not stop")
		}
	})
	return rt
}

func (h *Hosted) handle(ctx context.Context, inv durable.Invocation) error {
	key := inv.Run().Key
	h.mu.Lock()
	body := h.bodies[key]
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.returned[key]++
		h.mu.Unlock()
	}()
	return body(ctx, inv)
}

func (h *Hosted) Start(tb testing.TB, body Body) string {
	tb.Helper()
	h.mu.Lock()
	h.runs++
	key := fmt.Sprintf("turn-%d", h.runs)
	h.bodies[key] = body
	h.mu.Unlock()
	if _, err := h.rt.Start(tb.Context(), durable.StartRequest{Handler: hostedHandler, Key: key}); err != nil {
		tb.Fatalf("start %s: %v", key, err)
	}
	return key
}

func (h *Hosted) Returned(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.returned[key]
}

func (h *Hosted) AwaitEnded(tb testing.TB, key string, invocations int) {
	tb.Helper()
	deadline := time.NewTimer(hostedWait)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		status, err := h.rt.Status(tb.Context(), durable.RunRef{Key: key})
		if err != nil {
			tb.Fatalf("status of %s: %v", key, err)
		}
		if status.State != durable.RunRunning && h.Returned(key) == invocations {
			if status.State != durable.RunSucceeded {
				tb.Fatalf("run %s ended as %s (%s), want succeeded", key, status.State, status.Detail)
			}
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			tb.Fatalf("timed out waiting until %s ended after %d invocations (returned %d)", key, invocations, h.Returned(key))
		}
	}
}

func (h *Hosted) Run(tb testing.TB, body Body) string {
	tb.Helper()
	key := h.Start(tb, body)
	h.AwaitEnded(tb, key, 1)
	return key
}

func (h *Hosted) Preempt(tb testing.TB) {
	tb.Helper()
	if _, err := h.DB.ExecContext(tb.Context(), `DELETE FROM durable_host`); err != nil {
		tb.Fatalf("delete the host lease: %v", err)
	}
}

type Action struct {
	Key     string
	Outcome string
}

func (h *Hosted) Actions(tb testing.TB, key string) []Action {
	tb.Helper()
	rows, err := h.DB.QueryContext(tb.Context(), `SELECT action_key, outcome FROM durable_action WHERE run_key = ? ORDER BY id`, key)
	if err != nil {
		tb.Fatalf("read the actions of %s: %v", key, err)
	}
	defer func() { _ = rows.Close() }()
	var actions []Action
	for rows.Next() {
		var action Action
		if err := rows.Scan(&action.Key, &action.Outcome); err != nil {
			tb.Fatalf("scan an action of %s: %v", key, err)
		}
		actions = append(actions, action)
	}
	if err := rows.Err(); err != nil {
		tb.Fatalf("read the actions of %s: %v", key, err)
	}
	return actions
}

func InTurn(ctx context.Context, inv durable.Invocation) context.Context {
	return toolcall.With(durable.WithTurnScope(ctx, durable.NewTurnScope(inv)), toolcall.NewBatch(1, 1).Slot(0))
}

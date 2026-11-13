package runtimeengine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/channelhost"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
	"github.com/anggasct/aura/internal/store"
)

const ghostHostOwner = "ghost-host"

type terminalRecord struct {
	kind   string
	code   turn.ErrorCode
	detail string
}

func terminalRecordOf(t *testing.T, db *sql.DB, turnID string) terminalRecord {
	t.Helper()
	terminals := terminalEventsOf(t, db, turnID)
	if len(terminals) != 1 {
		t.Fatalf("terminal events of %s = %v, want one", turnID, eventIDs(terminals))
	}
	var payload struct {
		Code   turn.ErrorCode `json:"code"`
		Detail string         `json:"detail"`
	}
	if err := json.Unmarshal(terminals[0].Payload, &payload); err != nil {
		t.Fatalf("decode the terminal payload of %s: %v", turnID, err)
	}
	return terminalRecord{kind: terminals[0].Kind, code: payload.Code, detail: payload.Detail}
}

func deliveryRecordsOf(t *testing.T, db *sql.DB, turnID string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT CAST(result AS TEXT) FROM durable_action WHERE run_key = ? AND action_key = ?`, turnID, deliveryAction)
	if err != nil {
		t.Fatalf("read the delivery records of %s: %v", turnID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			t.Fatalf("scan a delivery record of %s: %v", turnID, err)
		}
		out = append(out, result)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the delivery records of %s: %v", turnID, err)
	}
	return out
}

func startDurableChannelTurn(t *testing.T, adapter runtimechannelhost.ChannelPort, executor TurnExecutor) (rig *durableHostRig, outcome <-chan turnOutcome) {
	t.Helper()
	rig = newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16}, executor)
	rig.engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{adapter})
	mustCreateSession(t, rig.db, "channel:333")
	rig.startHost()
	outcome = callTurn(context.WithoutCancel(t.Context()), rig.engine, channelTurnRequest("channel:333", "turn-1"))
	return rig, outcome
}

func runDurableChannelTurn(t *testing.T, adapter runtimechannelhost.ChannelPort, step turntest.FakeStep) *durableHostRig {
	t.Helper()
	rig, outcome := startDurableChannelTurn(t, adapter, turntest.NewFakeExecutor([]turntest.FakeStep{step}))
	select {
	case <-outcome:
	case <-time.After(hostRigTimeout):
		t.Fatal("the turn never ended")
	}
	return rig
}

type invocationContextExecutor struct {
	TurnExecutor
	contexts chan context.Context
}

func (x *invocationContextExecutor) Execute(ctx context.Context, req *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	select {
	case x.contexts <- ctx:
	default:
	}
	return x.TurnExecutor.Execute(ctx, req)
}

type stopOutlivingAdapter struct {
	capturingAdapter
	invocations <-chan context.Context
	entered     chan struct{}
}

func (a *stopOutlivingAdapter) Deliver(ctx context.Context, req *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	timeout := time.NewTimer(hostRigTimeout)
	defer timeout.Stop()
	var invocation context.Context
	select {
	case invocation = <-a.invocations:
	case <-timeout.C:
		return runtimechannelhost.ProviderReceipt{}, errors.New("the executor never saw the invocation")
	}
	close(a.entered)
	select {
	case <-invocation.Done():
	case <-timeout.C:
		return runtimechannelhost.ProviderReceipt{}, errors.New("the host never stopped the invocation")
	}
	if err := ctx.Err(); err != nil {
		return runtimechannelhost.ProviderReceipt{}, err
	}
	return a.capturingAdapter.Deliver(ctx, req)
}

type fencedDeliveryAdapter struct {
	capturingAdapter
	once   sync.Once
	db     *sql.DB
	handed chan struct{}
	err    error
}

func (a *fencedDeliveryAdapter) Deliver(ctx context.Context, req *runtimechannelhost.DeliveryRequest) (runtimechannelhost.ProviderReceipt, error) {
	a.once.Do(func() {
		a.err = handRunToGhostHost(ctx, a.db, req.EffectID)
		close(a.handed)
	})
	return a.capturingAdapter.Deliver(ctx, req)
}

func (a *fencedDeliveryAdapter) await(t *testing.T) {
	t.Helper()
	select {
	case <-a.handed:
	case <-time.After(hostRigTimeout):
		t.Fatal("the turn never reached its delivery")
	}
	if a.err != nil {
		t.Fatalf("hand the run to another host: %v", a.err)
	}
}

func TestDurableTurnJournalsItsDelivery(t *testing.T) {
	t.Run("a sent reply", func(t *testing.T) {
		capture := &capturingAdapter{}
		rig := runDurableChannelTurn(t, capture, textStep("hello"))
		if got := terminalRecordOf(t, rig.db, "turn-1"); got.kind != turn.EventKindTurnCompleted {
			t.Fatalf("terminal of turn-1 = %+v, want turn.completed", got)
		}
		if got := deliveryRecordsOf(t, rig.db, "turn-1"); len(got) != 1 || got[0] != `{"outcome":"sent"}` {
			t.Fatalf("delivery records = %v, want one sent record", got)
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("Deliver calls = %d, want 1", got)
		}
	})
	t.Run("a reply without text", func(t *testing.T) {
		capture := &capturingAdapter{}
		rig := runDurableChannelTurn(t, capture, turntest.FakeStep{Kind: turn.EventKindMessageCompleted, Payload: json.RawMessage(`{}`)})
		if got := terminalRecordOf(t, rig.db, "turn-1"); got.kind != turn.EventKindTurnCompleted {
			t.Fatalf("terminal of turn-1 = %+v, want turn.completed", got)
		}
		if got := deliveryRecordsOf(t, rig.db, "turn-1"); len(got) != 1 || got[0] != `{"outcome":"skipped"}` {
			t.Fatalf("delivery records = %v, want one skipped record", got)
		}
		if got := capture.count(); got != 0 {
			t.Fatalf("Deliver calls = %d, want 0", got)
		}
	})
	t.Run("a reply whose channel has no adapter", func(t *testing.T) {
		capture := &capturingAdapter{}
		rig := runDurableChannelTurn(t, &namedCapturingAdapter{capturingAdapter: capture, name: "telegram"}, textStep("hello"))
		if got := terminalRecordOf(t, rig.db, "turn-1"); got.kind != turn.EventKindTurnFailed || got.code != turn.ErrorCodeDeliveryFailed {
			t.Fatalf("terminal of turn-1 = %+v, want turn.failed with %s", got, turn.ErrorCodeDeliveryFailed)
		}
		want := `{"outcome":"failed","code":"delivery_failed","detail":"delivery_failed: owning channel adapter is not configured"}`
		if got := deliveryRecordsOf(t, rig.db, "turn-1"); len(got) != 1 || got[0] != want {
			t.Fatalf("delivery records = %v, want %s", got, want)
		}
		if got := capture.count(); got != 0 {
			t.Fatalf("Deliver calls = %d, want 0", got)
		}
	})
	t.Run("a delivery whose record another host fences is decided again by the next execution", func(t *testing.T) {
		var hooks atomic.Int32
		cfg := Config{MaxActiveTurns: 4, MaxPendingTurns: 16, PostTurnHook: func(context.Context, *turn.TurnRequest, []store.RuntimeEvent) error {
			hooks.Add(1)
			return nil
		}}
		executor := turntest.NewFakeExecutor([]turntest.FakeStep{textStep("hello")})
		rig := newDurableHostRig(t, cfg, executor)
		adapter := &fencedDeliveryAdapter{db: rig.db, handed: make(chan struct{})}
		rig.engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{adapter})
		mustCreateSession(t, rig.db, "channel:333")
		rig.startHost()
		_ = callTurn(context.WithoutCancel(t.Context()), rig.engine, channelTurnRequest("channel:333", "turn-1"))
		adapter.await(t)
		rig.stopHost()
		if got := hooks.Load(); got != 0 {
			t.Fatalf("post-turn hooks after the fenced delivery = %d, want 0", got)
		}
		if terminals := terminalsOf(t, rig.db, "turn-1"); len(terminals) != 0 {
			t.Fatalf("terminal events after the fenced delivery = %v, want none", terminals)
		}
		if got := deliveryRecordsOf(t, rig.db, "turn-1"); len(got) != 0 {
			t.Fatalf("delivery records after the fenced delivery = %v, want none", got)
		}
		if _, err := rig.db.ExecContext(t.Context(), `DELETE FROM durable_host WHERE owner = ?`, ghostHostOwner); err != nil {
			t.Fatalf("drop the other host: %v", err)
		}
		rig.build(executor)
		rig.engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{adapter})
		rig.startHost()
		awaitTerminals(t, rig.db, "turn-1")
		if got := terminalRecordOf(t, rig.db, "turn-1"); got.kind != turn.EventKindTurnCompleted {
			t.Fatalf("terminal of turn-1 = %+v, want turn.completed", got)
		}
		if got := deliveryRecordsOf(t, rig.db, "turn-1"); len(got) != 1 || got[0] != `{"outcome":"sent"}` {
			t.Fatalf("delivery records = %v, want one sent record", got)
		}
		if got := adapter.count(); got != 2 {
			t.Fatalf("Deliver calls across the two executions = %d, want 2", got)
		}
		if got := hooks.Load(); got != 1 {
			t.Fatalf("post-turn hooks across the two executions = %d, want 1", got)
		}
	})
	t.Run("a reply whose send outlives the host's stop", func(t *testing.T) {
		invocations := make(chan context.Context, 1)
		executor := &invocationContextExecutor{TurnExecutor: turntest.NewFakeExecutor([]turntest.FakeStep{textStep("hello")}), contexts: invocations}
		adapter := &stopOutlivingAdapter{invocations: invocations, entered: make(chan struct{})}
		rig, _ := startDurableChannelTurn(t, adapter, executor)
		select {
		case <-adapter.entered:
		case <-time.After(hostRigTimeout):
			t.Fatal("the turn never reached its delivery")
		}
		rig.stopHost()
		if got := terminalRecordOf(t, rig.db, "turn-1"); got.kind != turn.EventKindTurnCompleted {
			t.Fatalf("terminal of turn-1 = %+v, want turn.completed", got)
		}
		if got := deliveryRecordsOf(t, rig.db, "turn-1"); len(got) != 1 || got[0] != `{"outcome":"sent"}` {
			t.Fatalf("delivery records = %v, want one sent record", got)
		}
		if got := adapter.count(); got != 1 {
			t.Fatalf("Deliver calls = %d, want 1", got)
		}
	})
}

type lifeLogs struct {
	first  lockedBuffer
	replay lockedBuffer
}

func handRunToGhostHost(ctx context.Context, db *sql.DB, runKey string) error {
	now := time.Now()
	if _, err := db.ExecContext(ctx, `INSERT INTO durable_host (owner, expires_at, created_at) VALUES (?, ?, ?)`,
		ghostHostOwner, now.Add(time.Hour).UnixMilli(), now.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE durable_run SET owner = ? WHERE key = ?`, ghostHostOwner, runKey)
	return err
}

type crashAfterDelivery struct {
	once    sync.Once
	db      *sql.DB
	crashed chan struct{}
	err     error
}

func (c *crashAfterDelivery) hook(ctx context.Context, req *turn.TurnRequest, _ []store.RuntimeEvent) error {
	c.handOff(ctx, req.TurnID)
	return nil
}

func (c *crashAfterDelivery) handOff(ctx context.Context, runKey string) {
	c.once.Do(func() {
		c.err = handRunToGhostHost(ctx, c.db, runKey)
		close(c.crashed)
	})
}

func (c *crashAfterDelivery) await(t *testing.T) {
	t.Helper()
	select {
	case <-c.crashed:
	case <-time.After(hostRigTimeout):
		t.Fatal("the first life never reached its crash point")
	}
	if c.err != nil {
		t.Fatalf("hand the run to another host: %v", c.err)
	}
}

func replayAfterDelivery(t *testing.T, adapter runtimechannelhost.ChannelPort, logs *lifeLogs) *durableHostRig {
	t.Helper()
	db := openTestDatabase(t)
	crash := &crashAfterDelivery{db: db, crashed: make(chan struct{})}
	cfg := Config{MaxActiveTurns: 4, MaxPendingTurns: 16, PostTurnHook: crash.hook}
	rig := &durableHostRig{t: t, cfg: cfg, db: db, sessions: newHookedSessionStore(t, db)}
	executor := turntest.NewFakeExecutor([]turntest.FakeStep{textStep("hello once")})
	rig.build(executor)
	rig.engine.logger = slog.New(slog.NewTextHandler(&logs.first, nil))
	rig.engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{adapter})
	mustCreateSession(t, db, "channel:333")
	rig.startHost()
	_ = callTurn(context.WithoutCancel(t.Context()), rig.engine, channelTurnRequest("channel:333", "turn-1"))
	crash.await(t)
	rig.stopHost()
	if terminals := terminalsOf(t, db, "turn-1"); len(terminals) != 0 {
		t.Fatalf("terminal events after the first life = %v, want none", terminals)
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM durable_host WHERE owner = ?`, ghostHostOwner); err != nil {
		t.Fatalf("drop the other host: %v", err)
	}
	rig.build(executor)
	rig.engine.logger = slog.New(slog.NewTextHandler(&logs.replay, nil))
	rig.engine.SetChannelAdapters([]runtimechannelhost.ChannelPort{adapter})
	rig.startHost()
	awaitTerminals(t, db, "turn-1")
	return rig
}

func TestReplayedTurnDoesNotDeliverAgain(t *testing.T) {
	t.Run("a sent reply", func(t *testing.T) {
		capture := &capturingAdapter{}
		rig := replayAfterDelivery(t, capture, &lifeLogs{})
		if got := terminalRecordOf(t, rig.db, "turn-1"); got.kind != turn.EventKindTurnCompleted {
			t.Fatalf("terminal of turn-1 = %+v, want turn.completed", got)
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("Deliver calls across the restart = %d, want 1", got)
		}
	})
	t.Run("a refused reply", func(t *testing.T) {
		capture := &capturingAdapter{deliverErr: errDeliveryBoom}
		logs := &lifeLogs{}
		rig := replayAfterDelivery(t, capture, logs)
		if got := terminalRecordOf(t, rig.db, "turn-1"); got.kind != turn.EventKindTurnFailed || got.code != turn.ErrorCodeDeliveryFailed {
			t.Fatalf("terminal of turn-1 = %+v, want turn.failed with %s", got, turn.ErrorCodeDeliveryFailed)
		}
		if got := capture.count(); got != 1 {
			t.Fatalf("Deliver calls across the restart = %d, want 1", got)
		}
		if got := strings.Count(logs.first.String(), "turn delivery failed"); got != 1 {
			t.Errorf("the first life logged the failed delivery %d times, want once: %q", got, logs.first.String())
		}
		if strings.Contains(logs.replay.String(), "turn delivery failed") {
			t.Errorf("the replay logged the recorded failure again: %q", logs.replay.String())
		}
	})
}

func TestAmbiguousDeliveryEndsTheTurnAmbiguous(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		code   turn.ErrorCode
		detail string
	}{
		{name: "an error that fulfils the port's contract", err: fmt.Errorf("discord: %w", runtimechannelhost.ErrDeliveryAmbiguous), code: turn.ErrorCodeDeliveryAmbiguous, detail: "channel delivery outcome is unknown"},
		{name: "an error that does not", err: errDeliveryBoom, code: turn.ErrorCodeDeliveryFailed, detail: "channel delivery failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &capturingAdapter{deliverErr: tc.err}
			rig := runDurableChannelTurn(t, capture, textStep("hello"))
			got := terminalRecordOf(t, rig.db, "turn-1")
			if got.kind != turn.EventKindTurnFailed || got.code != tc.code {
				t.Fatalf("terminal of turn-1 = %+v, want turn.failed with %s", got, tc.code)
			}
			if want := tc.detail + ": " + tc.err.Error(); got.detail != want {
				t.Errorf("terminal detail = %q, want %q", got.detail, want)
			}
			records := deliveryRecordsOf(t, rig.db, "turn-1")
			if len(records) != 1 {
				t.Fatalf("delivery records = %v, want one", records)
			}
			var record deliveryRecord
			if err := json.Unmarshal([]byte(records[0]), &record); err != nil {
				t.Fatalf("decode the delivery record %s: %v", records[0], err)
			}
			if want := (deliveryRecord{Outcome: deliveryFailed, Code: tc.code, Detail: tc.err.Error()}); record != want {
				t.Fatalf("delivery record = %+v, want %+v", record, want)
			}
		})
	}
}

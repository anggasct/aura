package runtimeengine

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/turn"
)

var freshTurnStart = regexp.MustCompile(`^\{"started_at":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z","journal":2\}$`)

func turnStartResult(t *testing.T, rig *durableHostRig, turnID string) string {
	t.Helper()
	var raw string
	if err := rig.db.QueryRowContext(t.Context(), `SELECT CAST(result AS TEXT) FROM durable_action WHERE run_key = ? AND action_key = 'turn-start'`, turnID).Scan(&raw); err != nil {
		t.Fatalf("read the turn-start record of %s: %v", turnID, err)
	}
	return raw
}

func modelRows(t *testing.T, rig *durableHostRig, turnID string) int {
	t.Helper()
	var rows int
	if err := rig.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM durable_action WHERE run_key = ? AND action_key = 'model-1'`, turnID).Scan(&rows); err != nil {
		t.Fatalf("count the model rows of %s: %v", turnID, err)
	}
	return rows
}

func seedTurnStart(t *testing.T, rig *durableHostRig, turnID, record string) {
	t.Helper()
	eventually(t, "the run of "+turnID+" exists", func() bool {
		var runs int
		if err := rig.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM durable_run WHERE key = ?`, turnID).Scan(&runs); err != nil {
			t.Fatalf("count the runs of %s: %v", turnID, err)
		}
		return runs == 1
	})
	if _, err := rig.db.ExecContext(t.Context(), `INSERT INTO durable_action (run_key, action_key, outcome, result, detail, created_at) VALUES (?, 'turn-start', 'result', ?, '', ?)`, turnID, []byte(record), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed the turn-start record of %s: %v", turnID, err)
	}
}

func TestTurnJournalOfAnotherVersionIsRefused(t *testing.T) {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	cases := []struct {
		name   string
		record string
	}{
		{name: "a bare instant", record: `"` + stamp + `"`},
		{name: "the previous journal version", record: `{"started_at":"` + stamp + `","journal":1}`},
		{name: "another journal version", record: `{"started_at":"` + stamp + `","journal":0}`},
		{name: "an unknown field", record: `{"started_at":"` + stamp + `","journal":2,"extra":true}`},
		{name: "trailing data", record: `{"started_at":"` + stamp + `","journal":2} {}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			executor := turn.NewFakeExecutor([]turn.FakeStep{textStep("hello")})
			rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16, TurnTimeout: time.Hour}, executor)
			mustCreateSession(t, rig.db, "session-a")
			outcome := callTurn(context.WithoutCancel(t.Context()), rig.engine, sampleRequest("session-a", "turn-1"))
			seedTurnStart(t, rig, "turn-1", tc.record)
			rig.startHost()

			result := <-outcome
			if result.err != nil {
				t.Fatalf("turn stream: %v", result.err)
			}
			if got := lastKind(result.events); got != turn.EventKindTurnFailed {
				t.Fatalf("turn ended with %q, want turn.failed", got)
			}
			var payload struct {
				Code   turn.ErrorCode `json:"code"`
				Detail string         `json:"detail"`
			}
			if err := json.Unmarshal(result.events[len(result.events)-1].Payload, &payload); err != nil {
				t.Fatalf("decode the terminal payload: %v", err)
			}
			if payload.Code != turn.ErrorCodeRuntimeInternal || !strings.Contains(payload.Detail, "turn journal was written by another build") {
				t.Errorf("terminal = %q %q, want runtime_internal and the other build", payload.Code, payload.Detail)
			}
			if starts := executor.StartCount(); starts != 0 {
				t.Errorf("executor starts = %d, want 0", starts)
			}
			if rows := modelRows(t, rig, "turn-1"); rows != 0 {
				t.Errorf("model-1 rows = %d, want 0", rows)
			}
		})
	}
	t.Run("a fresh turn writes the current record", func(t *testing.T) {
		rig := newDurableHostRig(t, Config{MaxActiveTurns: 4, MaxPendingTurns: 16, TurnTimeout: time.Hour}, turn.NewFakeExecutor([]turn.FakeStep{textStep("hello")}))
		mustCreateSession(t, rig.db, "session-a")
		rig.startHost()
		events, err := collect(t, rig.engine, sampleRequest("session-a", "turn-1"))
		if err != nil {
			t.Fatalf("turn stream: %v", err)
		}
		if got := lastKind(events); got != turn.EventKindTurnCompleted {
			t.Fatalf("turn ended with %q, want turn.completed", got)
		}
		if raw := turnStartResult(t, rig, "turn-1"); !freshTurnStart.MatchString(raw) {
			t.Errorf("turn-start record = %s, want {\"started_at\":<instant>,\"journal\":2}", raw)
		}
	})
}

//go:build durable

package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/turn"
)

func TestDurableTurnCancelActiveCancelsTheRunningTurnOnly(t *testing.T) {
	plan, db := seedSuite(t, "cancel")
	life := openLife(t, plan)
	running, queued := &life.scenario.requests[0], &life.scenario.requests[1]
	runningDone := life.submit(t, running)
	waitUntil(t, "tool-a is entered", life.ledger.exists(t, "tool-a.entered"))
	queuedDone := life.submit(t, queued)
	life.waitActivity(t, running.TurnID, 1)

	turnID, cancelled, err := life.engine.CancelActive(t.Context(), suiteSession)
	if err != nil || !cancelled || turnID != running.TurnID {
		t.Fatalf("CancelActive = (%q, %v, %v), want (%q, true, nil)", turnID, cancelled, err, running.TurnID)
	}
	waitTerminal(t, db, running.TurnID)
	waitTerminal(t, db, queued.TurnID)
	awaitStream(t, runningDone)
	awaitStream(t, queuedDone)
	life.waitActivity(t, "", 0)

	terminals := terminalEvents(turnEvents(t, db, running.TurnID))
	if len(terminals) != 1 || terminals[0].Kind != turn.EventKindTurnCancelled {
		t.Errorf("terminal events of %s = %+v, want one %s", running.TurnID, terminals, turn.EventKindTurnCancelled)
	}
	waitUntil(t, "the run of "+running.TurnID+" ends", func() bool {
		state, _ := runRow(t, db, running.TurnID)
		return state != string(durable.RunRunning)
	})
	if state, _ := runRow(t, db, running.TurnID); state != string(durable.RunCancelled) {
		t.Errorf("run %s state = %q, want %q", running.TurnID, state, durable.RunCancelled)
	}
	requireCompletedOnce(t, turnEvents(t, db, queued.TurnID))
}

func TestDurableTurnFailsOnTransientSkillRender(t *testing.T) {
	plan, db := seedSuite(t, "transient-skill")
	life := openLife(t, plan)
	pinned, follow := &life.scenario.requests[0], &life.scenario.requests[1]

	awaitStream(t, life.submit(t, pinned))
	events := waitTerminal(t, db, pinned.TurnID)
	last := events[len(events)-1]
	var failed struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(last.Payload, &failed); err != nil {
		t.Fatalf("decode the terminal payload: %v", err)
	}
	if last.Kind != turn.EventKindTurnFailed || failed.Code != string(turn.ErrorCodeStorageUnavailable) {
		t.Errorf("terminal event = %s %s, want %s with the code %s", last.Kind, last.Payload, turn.EventKindTurnFailed, turn.ErrorCodeStorageUnavailable)
	}
	if n := countKind(events, turn.EventKindSkillActivated); n != 0 {
		t.Errorf("skill.activated events = %d, want 0", n)
	}
	life.ledger.requireCount(t, modelCalls, 0)

	awaitStream(t, life.submit(t, follow))
	requireCompletedOnce(t, waitTerminal(t, db, follow.TurnID))
}

func TestDurableTurnKnowledgeFailureKeepsTheTurn(t *testing.T) {
	plan, db := seedSuite(t, "knowledge-failure")
	life := openLife(t, plan)
	req := &life.scenario.requests[0]

	awaitStream(t, life.submit(t, req))
	events := waitTerminal(t, db, req.TurnID)
	requireCompletedOnce(t, events)
	requests := life.ledger.records(t, modelCalls)
	if len(requests) != 1 {
		t.Errorf("model calls = %d, want 1", len(requests))
	}
	for i, rec := range requests {
		if strings.Contains(rec.Text, turn.KnowledgeContextStart) {
			t.Errorf("model request %d holds a knowledge block:\n%s", i+1, rec.Text)
		}
	}
	if n := countKind(events, turn.EventKindKnowledgeRetrieved); n != 1 {
		t.Errorf("kb.retrieved.v1 events = %d, want 1", n)
	}
}

func TestDurableTurnReplaysAfterItsHostLosesTheLease(t *testing.T) {
	plan, db := seedSuite(t, "lost-lease")
	life := openLife(t, plan)
	req := &life.scenario.requests[0]

	done := life.submit(t, req)
	waitUntil(t, "tool-a is entered", life.ledger.exists(t, "tool-a.entered"))
	if _, err := db.ExecContext(t.Context(), `DELETE FROM durable_host`); err != nil {
		t.Fatalf("delete the host lease: %v", err)
	}
	waitUntil(t, "the run of "+req.TurnID+" is claimed again", func() bool {
		_, claim := runRow(t, db, req.TurnID)
		return claim >= 2
	})
	life.ledger.mark(t, "tool-a.release")

	awaitStream(t, done)
	requireCompletedOnce(t, waitTerminal(t, db, req.TurnID))
	life.ledger.requireCount(t, modelCalls, 2)
	life.ledger.requireCount(t, executions("tool-a"), 1)
}

//go:build durable

package restate

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/runtime"
)

func TestLiveCancelActiveCancelsTheRunningTurnOnly(t *testing.T) {
	binary := liveRestateBinary(t)
	model := &liveScriptModel{steps: []liveModelStep{{tool: "tool-a"}, {text: "next done"}}}
	rig := startLiveTurnRig(t, binary, model, []liveToolDef{{name: "tool-a"}})
	entered := filepath.Join(t.TempDir(), "tool-a-entered")
	rig.tools.gates["tool-a:entered"] = entered
	rig.tools.gates["tool-a"] = filepath.Join(t.TempDir(), "tool-a-never-released")

	streams := make(chan error, 2)
	run := func(turnID string) {
		go func() {
			for _, err := range rig.engine.Run(context.Background(), liveTurnRequest(turnID)) {
				if err != nil {
					streams <- err
					return
				}
			}
			streams <- nil
		}()
	}
	run("turn-running")
	rig.waitFile(entered, 90*time.Second)
	run("turn-queued")
	deadline := time.Now().Add(30 * time.Second)
	for {
		active, queued := rig.engine.SessionActivity("session-1")
		if active == "turn-running" && queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SessionActivity = %q queued %d, want turn-running queued 1", active, queued)
		}
		time.Sleep(100 * time.Millisecond)
	}

	turnID, cancelled, err := rig.engine.CancelActive(context.Background(), "session-1")
	if err != nil || !cancelled || turnID != "turn-running" {
		t.Fatalf("CancelActive = %q %v %v, want turn-running", turnID, cancelled, err)
	}
	running := rig.waitTerminal("turn-running", 60*time.Second)
	if last := running[len(running)-1]; last.Kind != runtime.EventKindTurnCancelled {
		t.Fatalf("running turn terminal = %q, want turn.cancelled", last.Kind)
	}
	queued := rig.waitTerminal("turn-queued", 120*time.Second)
	if last := queued[len(queued)-1]; last.Kind != runtime.EventKindTurnCompleted {
		t.Fatalf("queued turn terminal = %q, want turn.completed", last.Kind)
	}
	for range 2 {
		select {
		case err := <-streams:
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("stream never terminated")
		}
	}
	if got := rig.countKind("turn-running", runtime.EventKindTurnCancelled); got != 1 {
		t.Errorf("cancel terminals = %d, want 1", got)
	}
	drained := time.Now().Add(10 * time.Second)
	for {
		active, queuedCount := rig.engine.SessionActivity("session-1")
		if active == "" && queuedCount == 0 {
			return
		}
		if time.Now().After(drained) {
			t.Fatalf("SessionActivity after drain = %q %d", active, queuedCount)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

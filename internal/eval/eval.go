// Package eval provides Aura's deterministic evaluation harness: golden
// runtime-event trajectory checks and an adversarial corpus that fails on
// unauthorized tool use or privilege elevation. The corpus is data, so new
// cases are added without changing the runner.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/engine"
	"github.com/anggasct/aura/internal/store"
)

// SessionID is the session the harness creates and drives turns against.
const SessionID = "eval-session"

// Trajectory is one golden run: a scripted turn and the exact event-kind
// sequence it must produce, in order.
type Trajectory struct {
	Name      string
	Script    []runtime.FakeStep
	WantKinds []string
}

// AbuseCase is one adversarial tool request that policy must deny.
type AbuseCase struct {
	Name    string
	Request approval.ToolRequest
}

// ScriptedRuntime builds a runtime over a fresh store under dir that runs
// script for every turn, with the eval session pre-created. The returned
// clean func releases the store.
func ScriptedRuntime(ctx context.Context, dir string, script []runtime.FakeStep) (runtime.AgentRuntime, func(), error) {
	db, err := store.OpenDB(ctx, filepath.Join(dir, "aura.db"))
	if err != nil {
		return nil, nil, fmt.Errorf("eval: open store: %w", err)
	}
	cleanup := func() { _ = db.Close() }
	if err := store.Migrate(ctx, db); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("eval: migrate: %w", err)
	}
	session := store.Session{ID: SessionID, OwnerID: "eval"}
	if err := store.NewSessionService(db).Create(ctx, &session); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("eval: create session: %w", err)
	}
	engine, err := runtimeengine.NewEngine(runtimeengine.Config{}, store.NewEventStore(db), store.NewDedupeStore(db), runtime.NewFakeExecutor(script), nil)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("eval: build engine: %w", err)
	}
	return engine, cleanup, nil
}

// RunTrajectory drives one turn through the runtime and returns the streamed
// events in order.
func RunTrajectory(ctx context.Context, rt runtime.AgentRuntime, turnID string) ([]store.RuntimeEvent, error) {
	req := &runtime.TurnRequest{TurnID: turnID, SessionID: SessionID, PrincipalID: "eval", Origin: runtime.OriginInternal}
	var events []store.RuntimeEvent
	for ev, err := range rt.Run(ctx, req) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

// CheckTrajectory compares the streamed event-kind sequence against the golden
// want, in order.
func CheckTrajectory(events []store.RuntimeEvent, want []string) error {
	got := make([]string, len(events))
	for i := range events {
		got[i] = events[i].Kind
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("trajectory mismatch: got %v, want %v", got, want)
	}
	return nil
}

// CheckDenied asserts the broker denies the request. Denial is fail-closed: an
// evaluation error or any non-allow outcome satisfies it; only an explicit
// allow is a violation.
func CheckDenied(ctx context.Context, broker runtime.ToolBroker, req *approval.ToolRequest) error {
	decision, err := broker.Evaluate(ctx, req)
	if err == nil && decision.Outcome == approval.OutcomeAllow {
		return fmt.Errorf("tool %q with trust %q was allowed, want denied", req.ToolName, req.Trust)
	}
	return nil
}

type trajectoryFile struct {
	Name   string `json:"name"`
	Script []struct {
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	} `json:"script"`
	WantKinds []string `json:"want_kinds"`
}

// LoadTrajectories reads every *.json golden trajectory under dir. Reads are
// confined to dir through an os.Root, so a corpus entry cannot escape it.
func LoadTrajectories(dir string) ([]Trajectory, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("eval: open trajectories: %w", err)
	}
	defer func() { _ = root.Close() }()
	dirFile, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("eval: read trajectories dir: %w", err)
	}
	entries, err := dirFile.ReadDir(-1)
	_ = dirFile.Close()
	if err != nil {
		return nil, fmt.Errorf("eval: read trajectories dir: %w", err)
	}
	var out []Trajectory
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := root.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("eval: read %s: %w", entry.Name(), err)
		}
		var tf trajectoryFile
		if err := json.Unmarshal(data, &tf); err != nil {
			return nil, fmt.Errorf("eval: parse %s: %w", entry.Name(), err)
		}
		traj := Trajectory{Name: tf.Name, WantKinds: tf.WantKinds}
		for _, step := range tf.Script {
			payload := step.Payload
			if len(payload) == 0 {
				payload = json.RawMessage(`{}`)
			}
			traj.Script = append(traj.Script, runtime.FakeStep{Kind: step.Kind, Payload: payload})
		}
		out = append(out, traj)
	}
	return out, nil
}

type abuseFile struct {
	Name         string   `json:"name"`
	Tool         string   `json:"tool"`
	Trust        string   `json:"trust"`
	Capabilities []string `json:"capabilities"`
}

// LoadAbuseCases reads every *.json adversarial case under dir. Reads are
// confined to dir through an os.Root.
func LoadAbuseCases(dir string) ([]AbuseCase, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("eval: open abuse cases: %w", err)
	}
	defer func() { _ = root.Close() }()
	dirFile, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("eval: read abuse dir: %w", err)
	}
	entries, err := dirFile.ReadDir(-1)
	_ = dirFile.Close()
	if err != nil {
		return nil, fmt.Errorf("eval: read abuse dir: %w", err)
	}
	var out []AbuseCase
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := root.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("eval: read %s: %w", entry.Name(), err)
		}
		var af abuseFile
		if err := json.Unmarshal(data, &af); err != nil {
			return nil, fmt.Errorf("eval: parse %s: %w", entry.Name(), err)
		}
		out = append(out, AbuseCase{
			Name: af.Name,
			Request: approval.ToolRequest{
				RequestID:    "eval-" + strings.ReplaceAll(af.Name, " ", "-"),
				TurnID:       "eval-turn",
				SessionID:    SessionID,
				PrincipalID:  "eval",
				ToolName:     af.Tool,
				Trust:        approval.TrustLabel(af.Trust),
				Capabilities: af.Capabilities,
			},
		})
	}
	return out, nil
}

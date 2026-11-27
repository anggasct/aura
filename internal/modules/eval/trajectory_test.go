package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/engine"
	"github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
)

type Trajectory struct {
	Name      string
	Script    []turntest.FakeStep
	WantKinds []string
}

func ScriptedRuntime(ctx context.Context, dir string, script []turntest.FakeStep) (turn.AgentRuntime, func(), error) {
	db, err := sqlite.Open(ctx, filepath.Join(dir, "aura.db"), sqlite.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("eval: open store: %w", err)
	}
	cleanup := func() { _ = db.Close() }
	migrations, err := sqlite.History(turnstore.Schema())
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("eval: baseline: %w", err)
	}
	if err := sqlite.Migrate(ctx, db, migrations); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("eval: migrate: %w", err)
	}
	session := turn.Session{ID: SessionID, OwnerID: "eval"}
	if err := turnstore.NewSessionService(db).Create(ctx, &session); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("eval: create session: %w", err)
	}
	engine, err := runtimeengine.New(&runtimeengine.Deps{Events: turnstore.NewEventStore(db), Dedupe: turnstore.NewDedupeStore(db), Executor: turntest.NewFakeExecutor(script), Config: runtimeengine.Default()})
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("eval: build engine: %w", err)
	}
	return engine, cleanup, nil
}

func RunTrajectory(ctx context.Context, rt turn.AgentRuntime, turnID string) ([]turn.Event, error) {
	req := &turn.TurnRequest{TurnID: turnID, SessionID: SessionID, PrincipalID: "eval", Origin: turn.OriginInternal}
	var events []turn.Event
	for ev, err := range rt.Run(ctx, req) {
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func CheckTrajectory(events []turn.Event, want []string) error {
	got := make([]string, len(events))
	for i := range events {
		got[i] = events[i].Kind
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("trajectory mismatch: got %v, want %v", got, want)
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
			traj.Script = append(traj.Script, turntest.FakeStep{Kind: step.Kind, Payload: payload})
		}
		out = append(out, traj)
	}
	return out, nil
}

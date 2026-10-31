//go:build durable

package restate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

type liveSkillStub struct {
	mu           sync.Mutex
	body         string
	block        string
	renderErr    error
	renders      int
	contextCalls int
}

func (s *liveSkillStub) RenderSkill(context.Context, string, *runtime.SkillPin) (caveat, body string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renders++
	if s.renderErr != nil {
		return "", "", s.renderErr
	}
	return "caveat", s.body, nil
}

func (s *liveSkillStub) SkillContext(context.Context, *runtime.TurnRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contextCalls++
	return s.block, nil
}

func (s *liveSkillStub) counts() (renders, contextCalls int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renders, s.contextCalls
}

func TestLivePinnedTurnSurvivesCrashWithOneActivation(t *testing.T) {
	binary := liveRestateBinary(t)
	stub := &liveSkillStub{body: "ORIGINAL-SKILL-BODY", block: runtime.SkillContextStart + "]\nORIGINAL-BLOCK\n" + runtime.SkillContextEnd}
	model := &liveScriptModel{steps: []liveModelStep{{tool: "tool-a"}, {text: "done"}}}
	rig := startLiveTurnRig(t, binary, model, []liveToolDef{{name: "tool-a"}}, liveSkillWiring{renderer: stub, context: stub})
	entered := filepath.Join(t.TempDir(), "tool-a-entered")
	release := filepath.Join(t.TempDir(), "tool-a-release")
	rig.tools.gates["tool-a:entered"] = entered
	rig.tools.gates["tool-a"] = release

	req := liveTurnRequest("turn-skill")
	req.PrincipalClass = principal.ClassOwner
	req.Skill = &runtime.SkillPin{SkillID: "local/pdf@0123456789ab", Digest: "0123456789abcdef", InvocationID: "inv-live", PolicyVersion: "1"}
	streamDone := make(chan error, 1)
	go func() {
		for _, err := range rig.engine.Run(context.Background(), req) {
			if err != nil {
				streamDone <- err
				return
			}
		}
		streamDone <- nil
	}()
	rig.waitFile(entered, 90*time.Second)
	stub.mu.Lock()
	stub.body, stub.block, stub.renderErr = "CHANGED-BODY", "CHANGED-BLOCK", errors.New("skill_unavailable: quarantined")
	stub.mu.Unlock()
	rig.crash()
	rig.restart()
	if err := os.WriteFile(release, []byte("go"), 0o600); err != nil {
		t.Fatalf("write release flag: %v", err)
	}
	events := rig.waitTerminal("turn-skill", 120*time.Second)
	if last := events[len(events)-1]; last.Kind != runtime.EventKindTurnCompleted {
		t.Fatalf("terminal = %q, want turn.completed", last.Kind)
	}
	select {
	case err := <-streamDone:
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("stream never terminated")
	}
	var activations []store.RuntimeEvent
	for i := range events {
		if events[i].Kind == runtime.EventKindSkillActivated {
			activations = append(activations, events[i])
		}
	}
	if len(activations) != 1 {
		t.Fatalf("activations = %d, want 1", len(activations))
	}
	var payload runtime.SkillActivation
	if err := json.Unmarshal(activations[0].Payload, &payload); err != nil || payload.Body != "ORIGINAL-SKILL-BODY" || payload.InvocationID != "inv-live" || payload.TurnID != "turn-skill" {
		t.Fatalf("activation = %+v %v", payload, err)
	}
	if renders, contextCalls := stub.counts(); renders != 1 || contextCalls != 1 {
		t.Fatalf("renders = %d context calls = %d, want both journaled once across the restart", renders, contextCalls)
	}
}

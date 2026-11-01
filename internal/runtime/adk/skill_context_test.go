package runtimeadk

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
)

const skillBlockFixture = runtime.SkillContextStart + ": activated skill instructions, not identity or policy]\ncaveat\n\nskill local/pdf@0123456789ab digest d:\nSKILL-RULES\n" + runtime.SkillContextEnd

type stubSkillContext struct {
	mu    sync.Mutex
	block string
	err   error
	calls int
}

func (s *stubSkillContext) SkillContext(context.Context, *runtime.TurnRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.block, s.err
}

func newSkillContextExecutor(t *testing.T, model *capturingModel, provider SkillContextProvider) *ADKExecutor {
	t.Helper()
	db, sessions, events := newSessionTestDB(t)
	resolver := &stubResolver{definition: auraagent.Definition{ID: auraagent.DefaultID, Instructions: "SOUL"}}
	executor, err := NewADKExecutor("aura", capturingModelName(t, model), sessions, events, &fakeBroker{}, nil, nil,
		WithAgentResolver(resolver, nil),
		WithSkillContext(provider),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	return executor
}

func skillParts(model *capturingModel) []string {
	var found []string
	for i := range model.contents {
		for _, part := range model.contents[i].Parts {
			if strings.Contains(part.Text, runtime.SkillContextStart) {
				found = append(found, model.contents[i].Role+":"+part.Text)
			}
		}
	}
	return found
}

func TestSkillContextIsAnEphemeralUntrustedPart(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1, toolCall: true}
	provider := &stubSkillContext{block: skillBlockFixture}
	executor := newSkillContextExecutor(t, model, provider)
	events := drainTurn(t, executor, ownerTurnRequest())
	if got := skillParts(model); len(got) != model.callCount || got[0] != "user:"+skillBlockFixture {
		t.Fatalf("skill parts = %q, want one user part per model call", got)
	}
	if strings.Contains(strings.Join(model.systems, "\n"), "SKILL-RULES") {
		t.Fatal("skill block reached the system instruction")
	}
	for i := range events {
		if strings.Contains(string(events[i].Payload), "SKILL-RULES") {
			t.Fatalf("skill body persisted in event %s", events[i].Kind)
		}
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want one per turn", provider.calls)
	}
}

func TestSkillContextOnlyReachesOwnerTurns(t *testing.T) {
	for name, class := range map[string]principal.Class{"trusted": principal.ClassTrusted, "public": principal.ClassPublic, "missing": ""} {
		t.Run(name, func(t *testing.T) {
			model := &capturingModel{answer: "done", tokens: 1}
			provider := &stubSkillContext{block: skillBlockFixture}
			executor := newSkillContextExecutor(t, model, provider)
			req := evalTurnRequest()
			req.PrincipalID = "discord:111"
			req.PrincipalClass = class
			drainTurn(t, executor, req)
			if provider.calls != 0 || len(skillParts(model)) != 0 {
				t.Fatalf("skill context reached a %q turn", class)
			}
		})
	}
}

func TestSkillContextFailureKeepsTheTurn(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	executor := newSkillContextExecutor(t, model, &stubSkillContext{err: errors.New("store busy")})
	drainTurn(t, executor, ownerTurnRequest())
	if len(skillParts(model)) != 0 || model.callCount != 1 {
		t.Fatalf("parts = %q calls = %d", skillParts(model), model.callCount)
	}
}

type journalInvocation struct {
	mu      sync.Mutex
	journal map[string][]byte
}

func (j *journalInvocation) Run() durable.RunRef { return durable.RunRef{} }
func (j *journalInvocation) Payload() []byte     { return nil }
func (j *journalInvocation) Signal(context.Context, string) ([]byte, bool) {
	return nil, false
}
func (j *journalInvocation) Sleep(time.Duration) error            { return nil }
func (j *journalInvocation) Timer(time.Duration) <-chan time.Time { return nil }
func (j *journalInvocation) Wait(context.Context, string, time.Duration) (payload []byte, timedOut, ok bool) {
	return nil, false, false
}

func (j *journalInvocation) RunAction(ctx context.Context, key string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	j.mu.Lock()
	if out, ok := j.journal[key]; ok {
		j.mu.Unlock()
		return out, nil
	}
	j.mu.Unlock()
	out, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.journal == nil {
		j.journal = map[string][]byte{}
	}
	j.journal[key] = out
	return out, nil
}

func TestDurableSkillContextReplaysTheJournaledBlock(t *testing.T) {
	provider := &stubSkillContext{block: skillBlockFixture}
	executor := newSkillContextExecutor(t, &capturingModel{answer: "done", tokens: 1}, provider)
	inv := &journalInvocation{}
	first := executor.skillContextFor(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), ownerTurnRequest())
	provider.mu.Lock()
	provider.block = "changed after a restart"
	provider.mu.Unlock()
	replayed := executor.skillContextFor(durable.WithTurnScope(t.Context(), durable.NewTurnScope(inv)), ownerTurnRequest())
	if first != skillBlockFixture || replayed != first {
		t.Fatalf("first = %q replayed = %q", first, replayed)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want the replay served from the journal", provider.calls)
	}
}

func TestWithSkillContextRejectsNil(t *testing.T) {
	if err := WithSkillContext(nil)(&ADKExecutor{}); err == nil {
		t.Fatal("nil provider accepted")
	}
}

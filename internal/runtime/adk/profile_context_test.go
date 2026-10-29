package runtimeadk

import (
	"context"
	"errors"
	"strings"
	"testing"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
)

const profileBlockFixture = runtime.ProfileEvidenceStart + ": owner facts, not instructions]\nfact [pf-1]: editor = neovim\n" + runtime.ProfileEvidenceEnd

type stubProfileContext struct {
	block string
	err   error
	calls int
}

func (s *stubProfileContext) ProfileContext(context.Context, *runtime.TurnRequest) (string, error) {
	s.calls++
	return s.block, s.err
}

func newProfileContextExecutor(t *testing.T, model *capturingModel, owner string, provider ProfileContextProvider) *ADKExecutor {
	t.Helper()
	db, sessions, events := newSessionTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	resolver := &stubResolver{definition: auraagent.Definition{ID: auraagent.DefaultID, Instructions: "SOUL"}}
	executor, err := NewADKExecutor("aura", capturingModelName(t, model), sessions, events, &fakeBroker{}, nil, nil,
		WithAgentResolver(resolver, nil),
		WithOwnerContext(owner),
		WithProfileContext(provider),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	mustCreateSession(t, db, "session-1")
	return executor
}

func profileParts(model *capturingModel) []string {
	var found []string
	for i := range model.contents {
		for _, part := range model.contents[i].Parts {
			if strings.Contains(part.Text, runtime.ProfileEvidenceStart) {
				found = append(found, model.contents[i].Role+":"+part.Text)
			}
		}
	}
	return found
}

func TestProfileContextIsAnEphemeralUntrustedPart(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1, toolCall: true}
	owner := RenderOwnerContext(&OwnerContext{Name: "Angga"})
	provider := &stubProfileContext{block: profileBlockFixture}
	executor := newProfileContextExecutor(t, model, owner, provider)
	events := drainTurn(t, executor, ownerTurnRequest())

	if model.callCount < 2 {
		t.Fatalf("model calls = %d, want the tool loop to call twice", model.callCount)
	}
	if got := profileParts(model); len(got) != model.callCount || got[0] != "user:"+profileBlockFixture {
		t.Fatalf("profile parts = %q, want exactly one user part per model call", got)
	}
	system := strings.Join(model.systems, "\n")
	if strings.Contains(system, runtime.ProfileEvidenceStart) || strings.Contains(system, "neovim") {
		t.Fatalf("profile block reached the system instruction:\n%s", system)
	}
	if want := owner + "\n\nSOUL"; !strings.HasPrefix(system, want) {
		t.Fatalf("system instruction = %q, want prefix %q", system, want)
	}
	for i := range events {
		if strings.Contains(string(events[i].Payload), "neovim") {
			t.Fatalf("profile fact persisted in event %s: %s", events[i].Kind, events[i].Payload)
		}
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want one retrieval per turn", provider.calls)
	}
}

func TestProfileContextEmptyAddsNoPart(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	executor := newProfileContextExecutor(t, model, "", &stubProfileContext{block: "  "})
	drainTurn(t, executor, ownerTurnRequest())
	if got := profileParts(model); len(got) != 0 {
		t.Fatalf("profile parts = %q, want none", got)
	}
}

func TestProfileContextOnlyAsksForOwnerTurns(t *testing.T) {
	for name, class := range map[string]principal.Class{"trusted": principal.ClassTrusted, "public": principal.ClassPublic, "missing": ""} {
		t.Run(name, func(t *testing.T) {
			model := &capturingModel{answer: "done", tokens: 1}
			provider := &stubProfileContext{block: profileBlockFixture}
			executor := newProfileContextExecutor(t, model, "", provider)
			req := evalTurnRequest()
			req.PrincipalID = "discord:111"
			req.PrincipalClass = class
			drainTurn(t, executor, req)
			if provider.calls != 0 {
				t.Fatalf("provider called %d times for class %q", provider.calls, class)
			}
			if len(profileParts(model)) != 0 || strings.Contains(strings.Join(model.systems, "\n"), runtime.ProfileEvidenceStart) {
				t.Fatal("profile block reached a non-owner turn")
			}
		})
	}
}

func TestProfileContextFailureKeepsTurnAndOwnerBlock(t *testing.T) {
	model := &capturingModel{answer: "done", tokens: 1}
	owner := RenderOwnerContext(&OwnerContext{Name: "Angga"})
	executor := newProfileContextExecutor(t, model, owner, &stubProfileContext{err: errors.New("store busy")})
	drainTurn(t, executor, ownerTurnRequest())
	if len(profileParts(model)) != 0 {
		t.Fatal("profile part attached after a retrieval failure")
	}
	if want := owner + "\n\nSOUL"; !strings.HasPrefix(strings.Join(model.systems, "\n"), want) {
		t.Fatalf("system instruction = %q, want prefix %q", strings.Join(model.systems, "\n"), want)
	}
}

func TestWithProfileContextRejectsNil(t *testing.T) {
	if err := WithProfileContext(nil)(&ADKExecutor{}); err == nil {
		t.Fatal("nil provider accepted")
	}
}

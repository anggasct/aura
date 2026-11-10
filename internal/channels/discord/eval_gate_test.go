package discord

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/config"
)

type fakeEvalGate struct {
	mu        sync.Mutex
	evaluated map[string]bool
	err       error
	calls     []string
}

func (g *fakeEvalGate) Evaluated(_ context.Context, persona string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, persona)
	return g.evaluated[persona], g.err
}

func newEvalAdapter(t *testing.T, persona string, gate *fakeEvalGate) (*Adapter, *memorySink, *approvalStub, *accessObserver) {
	t.Helper()
	stub := newApprovalStub(t)
	adapter, sink, observer := newAccessAdapter(t, persona, []string{"discord:777"}, accessPersonas())
	settings := observer.settings()
	settings.Evals = gate
	adapter.SetAccess(&settings)
	adapter.restBase = stub.srv.URL
	return adapter, sink, stub, observer
}

func TestUnevaluatedPersonaRefusesNonOwnersOnly(t *testing.T) {
	gate := &fakeEvalGate{evaluated: map[string]bool{}}
	adapter, sink, stub, observer := newEvalAdapter(t, "lobby", gate)
	if admitted, err := adapter.admit(t.Context(), textMessage("9951", "4400", "", "666", "halo", nil)); err != nil || admitted {
		t.Fatalf("unevaluated public admit = %v, %v; want refused", admitted, err)
	}
	if sink.calls["message:9951"] != 0 {
		t.Fatal("refused message reached the ingress sink")
	}
	if got := postContents(stub, "4400"); len(got) != 1 || got[0] != config.PersonaMessageDefaults()[config.MessageUnavailable] {
		t.Fatalf("replies = %q, want the unavailable message", got)
	}
	if observer.count(observer.denied, DeniedReasonEvalMissing) != 1 {
		t.Fatal("eval refusal was not counted")
	}
	if admitted, err := adapter.admit(t.Context(), textMessage("9952", "4401", "", "777", "halo", nil)); err != nil || !admitted {
		t.Fatalf("owner admit = %v, %v; want admitted without an eval", admitted, err)
	}
	gate.evaluated["lobby"] = true
	if admitted, err := adapter.admit(t.Context(), textMessage("9953", "4400", "", "666", "halo", nil)); err != nil || !admitted {
		t.Fatalf("evaluated public admit = %v, %v; want admitted", admitted, err)
	}
	gate.err = errors.New("db down")
	if admitted, _ := adapter.admit(t.Context(), textMessage("9954", "4400", "", "666", "halo", nil)); admitted {
		t.Fatal("a failed eval lookup admitted a non-owner")
	}
}

func TestOwnerAudiencePersonasNeverConsultTheEvalGate(t *testing.T) {
	gate := &fakeEvalGate{evaluated: map[string]bool{}}
	adapter, _, _, _ := newEvalAdapter(t, "owneronly", gate)
	if admitted, err := adapter.admit(t.Context(), textMessage("9961", "4400", "", "777", "halo", nil)); err != nil || !admitted {
		t.Fatalf("owner admit = %v, %v", admitted, err)
	}
	if len(gate.calls) != 0 {
		t.Fatalf("owner-only persona consulted the eval gate: %v", gate.calls)
	}
}

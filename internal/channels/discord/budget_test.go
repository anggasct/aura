package discord

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/principal"
)

type fakeBudgetGate struct {
	mu      sync.Mutex
	refusal string
	err     error
	calls   []string
}

func (g *fakeBudgetGate) Admit(_ context.Context, principalID string, class principal.Class) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, principalID+"/"+string(class))
	return g.refusal, g.err
}

func newBudgetAdapter(t *testing.T, persona string, gate *fakeBudgetGate) (*Adapter, *memorySink, *approvalStub) {
	t.Helper()
	stub := newApprovalStub(t)
	adapter, sink, observer := newAccessAdapter(t, persona, []string{"discord:777"}, accessPersonas())
	settings := observer.settings()
	settings.Budgets = gate
	adapter.SetAccess(&settings)
	adapter.restBase = stub.srv.URL
	return adapter, sink, stub
}

func TestBudgetRefusalRepliesWithoutATurn(t *testing.T) {
	defaults := config.PersonaMessageDefaults()
	cases := map[string]struct {
		persona string
		author  string
		refusal string
		reply   string
		class   principal.Class
	}{
		"trusted rate limited":   {"team", "111", BudgetRefusalRateLimited, defaults[config.MessageRateLimited], principal.ClassTrusted},
		"trusted out of tokens":  {"team", "111", BudgetRefusalExhausted, defaults[config.MessageBudgetExhausted], principal.ClassTrusted},
		"public out of tokens":   {"lobby", "666", BudgetRefusalExhausted, defaults[config.MessageBudgetExhausted], principal.ClassPublic},
		"public rate limited dm": {"lobby", "666", BudgetRefusalRateLimited, defaults[config.MessageRateLimited], principal.ClassPublic},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gate := &fakeBudgetGate{refusal: tc.refusal}
			adapter, sink, stub := newBudgetAdapter(t, tc.persona, gate)
			admitted, err := adapter.admit(t.Context(), textMessage("9601", "4400", "", tc.author, "halo", nil))
			if err != nil || admitted {
				t.Fatalf("admit() = %v, %v; want a fixed reply, not a turn", admitted, err)
			}
			if sink.calls["message:9601"] != 0 {
				t.Fatal("refused message reached the ingress sink")
			}
			posts := stub.postsTo("4400")
			if len(posts) != 1 {
				t.Fatalf("replies = %d, want 1", len(posts))
			}
			if got, _ := posts[0].body["content"].(string); got != tc.reply {
				t.Fatalf("reply = %q, want %q", got, tc.reply)
			}
			if want := "discord:" + tc.author + "/" + string(tc.class); len(gate.calls) != 1 || gate.calls[0] != want {
				t.Fatalf("gate calls = %v, want [%s]", gate.calls, want)
			}
		})
	}
}

func TestBudgetGateSkipsOwnerAndAdmitsWithinLimits(t *testing.T) {
	gate := &fakeBudgetGate{refusal: BudgetRefusalExhausted}
	adapter, sink, _ := newBudgetAdapter(t, "team", gate)
	if admitted, err := adapter.admit(t.Context(), textMessage("9611", "4400", "", "777", "halo", nil)); err != nil || !admitted {
		t.Fatalf("owner admit() = %v, %v; want admitted without a budget check", admitted, err)
	}
	if len(gate.calls) != 0 {
		t.Fatalf("owner hit the budget gate: %v", gate.calls)
	}
	gate.refusal = ""
	if admitted, err := adapter.admit(t.Context(), textMessage("9612", "4400", "", "111", "halo", nil)); err != nil || !admitted {
		t.Fatalf("trusted admit() = %v, %v; want admitted within limits", admitted, err)
	}
	routedEnvelope(t, sink, "message:9612")
}

func TestBudgetGateFailureFailsClosedSilently(t *testing.T) {
	gate := &fakeBudgetGate{err: errors.New("ledger unavailable")}
	adapter, sink, stub := newBudgetAdapter(t, "team", gate)
	admitted, err := adapter.admit(t.Context(), textMessage("9621", "4400", "", "111", "halo", nil))
	if err != nil || admitted {
		t.Fatalf("admit() = %v, %v; want dropped", admitted, err)
	}
	if sink.calls["message:9621"] != 0 || len(stub.postsTo("4400")) != 0 {
		t.Fatal("failed budget check produced a turn or a reply")
	}
}

func TestBudgetRefusalUsesPersonaMessage(t *testing.T) {
	personas := accessPersonas()
	personas[1].Messages = map[string]string{config.MessageBudgetExhausted: "Kuota harian kamu sudah habis, coba lagi besok ya."}
	stub := newApprovalStub(t)
	adapter, _, observer := newAccessAdapter(t, "team", []string{"discord:777"}, personas)
	settings := observer.settings()
	settings.Budgets = &fakeBudgetGate{refusal: BudgetRefusalExhausted}
	adapter.SetAccess(&settings)
	adapter.restBase = stub.srv.URL
	if admitted, _ := adapter.admit(t.Context(), textMessage("9631", "4400", "", "111", "halo", nil)); admitted {
		t.Fatal("exhausted principal admitted")
	}
	posts := stub.postsTo("4400")
	if len(posts) != 1 {
		t.Fatalf("replies = %d, want 1", len(posts))
	}
	if got, _ := posts[0].body["content"].(string); got != "Kuota harian kamu sudah habis, coba lagi besok ya." {
		t.Fatalf("reply = %q, want the persona override", got)
	}
}

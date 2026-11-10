package discord

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/principal"
)

type fakePairing struct {
	mu       sync.Mutex
	granted  map[string]bool
	outcome  string
	requests []string
}

func (f *fakePairing) Granted(_ context.Context, principalID, account string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.granted[account+"/"+principalID], nil
}

func (f *fakePairing) Request(_ context.Context, principalID, account string) (PairingResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, account+"/"+principalID)
	if f.outcome == PairingOutcomeIssued {
		return PairingResult{Outcome: f.outcome, Code: "7K3M9QXR"}, nil
	}
	return PairingResult{Outcome: f.outcome}, nil
}

func pairingPersonas() []config.AgentDefinition {
	personas := accessPersonas()
	personas = append(personas,
		config.AgentDefinition{
			ID: "office", Description: "d", Instructions: "i", Audience: config.AudienceTrusted, Pairing: true,
			Messages: map[string]string{config.MessagePairingCode: "Kirim kode {code} ke pemilik ya."},
		},
	)
	return personas
}

func newPairingAdapter(t *testing.T, persona string, pairing *fakePairing) (*Adapter, *memorySink, *approvalStub) {
	t.Helper()
	stub := newApprovalStub(t)
	adapter, sink, observer := newAccessAdapter(t, persona, []string{"discord:777"}, pairingPersonas())
	settings := observer.settings()
	settings.Pairing = pairing
	adapter.SetAccess(&settings)
	adapter.restBase = stub.srv.URL
	return adapter, sink, stub
}

func TestUnknownDMToAPairingPersonaGetsAReply(t *testing.T) {
	cases := map[string]struct {
		outcome string
		reply   string
	}{
		"issued":  {PairingOutcomeIssued, "Kirim kode 7K3M9QXR ke pemilik ya."},
		"pending": {PairingOutcomePending, config.PersonaMessageDefaults()[config.MessagePairingPending]},
		"locked":  {PairingOutcomeLocked, config.PersonaMessageDefaults()[config.MessagePairingLocked]},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			pairing := &fakePairing{outcome: tc.outcome}
			adapter, sink, stub := newPairingAdapter(t, "office", pairing)
			admitted, err := adapter.admit(t.Context(), textMessage("9301", "4400", "", "666", "halo", nil))
			if err != nil || admitted {
				t.Fatalf("admit() = %v, %v; want a pairing reply, not a turn", admitted, err)
			}
			if sink.calls["message:9301"] != 0 {
				t.Fatal("pairing message reached the ingress sink")
			}
			posts := stub.postsTo("4400")
			if len(posts) != 1 {
				t.Fatalf("dm replies = %d, want 1", len(posts))
			}
			if got, _ := posts[0].body["content"].(string); got != tc.reply {
				t.Fatalf("reply = %q, want %q", got, tc.reply)
			}
			if len(pairing.requests) != 1 || pairing.requests[0] != "owner/discord:666" {
				t.Fatalf("pairing requests = %v", pairing.requests)
			}
		})
	}
}

func TestPairingStaysSilentOutsideItsScope(t *testing.T) {
	cases := map[string]struct {
		persona string
		message *messagePayload
	}{
		"trusted persona without pairing": {"team", textMessage("9401", "4400", "", "666", "halo", nil)},
		"owner-only persona":              {"owneronly", textMessage("9402", "4400", "", "666", "halo", nil)},
		"guild message":                   {"office", textMessage("9403", "333", "222", "666", "<@999> halo", []string{"999"})},
		"bot author": {"office", func() *messagePayload {
			m := textMessage("9404", "4400", "", "666", "halo", nil)
			m.Author.Bot = true
			return m
		}()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			pairing := &fakePairing{outcome: PairingOutcomeIssued}
			adapter, _, stub := newPairingAdapter(t, tc.persona, pairing)
			if admitted, err := adapter.admit(t.Context(), tc.message); err != nil || admitted {
				t.Fatalf("admit() = %v, %v; want dropped", admitted, err)
			}
			if len(pairing.requests) != 0 {
				t.Fatalf("pairing requests = %v, want none", pairing.requests)
			}
			if posts, _, _ := stub.counts(); posts != 0 {
				t.Fatalf("posts = %d, want silence", posts)
			}
		})
	}
}

func TestPairedPrincipalIsAdmittedAsTrustedUntilRevoked(t *testing.T) {
	pairing := &fakePairing{outcome: PairingOutcomePending, granted: map[string]bool{"owner/discord:666": true}}
	adapter, sink, stub := newPairingAdapter(t, "office", pairing)
	if admitted, err := adapter.admit(t.Context(), textMessage("9501", "4400", "", "666", "halo", nil)); err != nil || !admitted {
		t.Fatalf("paired admit() = %v, %v; want admitted", admitted, err)
	}
	env := routedEnvelope(t, sink, "message:9501")
	if env.PrincipalID != "discord:666" || env.PrincipalClass != string(principal.ClassTrusted) || env.AgentID != "office" {
		t.Fatalf("paired envelope = %q/%q/%q", env.PrincipalID, env.PrincipalClass, env.AgentID)
	}
	pairing.mu.Lock()
	pairing.granted = map[string]bool{}
	pairing.mu.Unlock()
	if admitted, err := adapter.admit(t.Context(), textMessage("9502", "4400", "", "666", "halo lagi", nil)); err != nil || admitted {
		t.Fatalf("revoked admit() = %v, %v; want dropped", admitted, err)
	}
	if len(pairing.requests) != 1 {
		t.Fatalf("pairing requests after revoke = %v, want one new request", pairing.requests)
	}
	posts := stub.postsTo("4400")
	content := ""
	if len(posts) == 1 {
		content, _ = posts[0].body["content"].(string)
	}
	if !strings.Contains(content, "pending") {
		t.Fatalf("revoked reply = %+v", posts)
	}
}

func TestPairingGrantIsScopedToTheAccount(t *testing.T) {
	pairing := &fakePairing{outcome: PairingOutcomePending, granted: map[string]bool{"home/discord:666": true}}
	adapter, sink, _ := newPairingAdapter(t, "office", pairing)
	if admitted, _ := adapter.admit(t.Context(), textMessage("9601", "4400", "", "666", "halo", nil)); admitted || sink.calls["message:9601"] != 0 {
		t.Fatal("a grant on another account admitted the principal")
	}
}

func TestOwnerAndAllowlistedSkipPairing(t *testing.T) {
	pairing := &fakePairing{outcome: PairingOutcomeIssued}
	adapter, sink, _ := newPairingAdapter(t, "office", pairing)
	for id, author := range map[string]string{"9701": "777", "9702": "111"} {
		if admitted, err := adapter.admit(t.Context(), textMessage(id, "4400", "", author, "halo", nil)); err != nil || !admitted {
			t.Fatalf("author %s admit() = %v, %v", author, admitted, err)
		}
		if sink.calls["message:"+id] != 1 {
			t.Fatalf("author %s did not reach the sink", author)
		}
	}
	if len(pairing.requests) != 0 {
		t.Fatalf("pairing requests = %v, want none for known principals", pairing.requests)
	}
}

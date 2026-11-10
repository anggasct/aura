package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"
)

func TestPublicPrincipalsInOneChannelNeverRecallEachOther(t *testing.T) {
	ctx := context.Background()
	db := newMemoryTestDB(t)
	sessions := store.NewSessionService(db)
	const (
		alice  = "discord:601"
		bob    = "discord:602"
		thread = "main/discord:owner/thread:7001"
	)
	channelSession := func(principalID string) string {
		return "main/discord:owner/channel:333/principal:" + principalID
	}
	for id, owner := range map[string]string{channelSession(alice): alice, channelSession(bob): bob, thread: alice} {
		if err := sessions.Create(ctx, &store.Session{ID: id, OwnerID: owner, Metadata: json.RawMessage(`{}`), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("create session %s: %v", id, err)
		}
	}
	service, err := memory.NewService(&memoryDocumentStore{store: store.NewMemoryStore(db)}, nil, &memory.Config{FallbackMaxDocuments: 5, FallbackTokenBudget: 2000})
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	hook := buildMemoryHook(service, nil, nil, nil)
	project := func(sessionID, principalID, eventID, text string) {
		t.Helper()
		turnRequest := &turn.TurnRequest{TurnID: "turn-" + eventID, SessionID: sessionID, PrincipalID: principalID, PrincipalClass: principal.ClassPublic, Origin: "discord:owner", AgentID: "main"}
		if err := hook(ctx, turnRequest, []store.RuntimeEvent{{
			ID: eventID, SessionID: sessionID, Sequence: 1, TurnID: turnRequest.TurnID, Author: "user",
			Kind: store.EventKindADK, SchemaVersion: 1, Payload: journalPayload(t, "user", text), CreatedAt: time.Now().UTC().Add(-time.Minute),
		}}); err != nil {
			t.Fatalf("hook(%s): %v", eventID, err)
		}
	}
	project(channelSession(alice), alice, "ev-alice", "my parcel tracking number is alpha")
	project(channelSession(bob), bob, "ev-bob", "my parcel tracking number is bravo")
	project(thread, bob, "ev-bob-thread", "my parcel tracking number is charlie")
	provider, err := memory.NewProvider(service, memory.ProviderConfig{Scopes: defaultRecallScopes(t)})
	if err != nil {
		t.Fatalf("NewProvider(): %v", err)
	}
	recalled := func(sessionID, principalID string) map[string]bool {
		t.Helper()
		evidence, err := provider.RecallTurn(ctx, &turn.TurnRequest{
			TurnID: "turn-recall", SessionID: sessionID, PrincipalID: principalID, PrincipalClass: principal.ClassPublic, Origin: "discord:owner", AgentID: "main",
			Parts: []runtimeingress.InputPart{{Text: "parcel tracking number"}},
		})
		if err != nil {
			t.Fatalf("RecallTurn(%s): %v", principalID, err)
		}
		sessionsSeen := map[string]bool{}
		if evidence != nil {
			for i := range evidence.Documents {
				sessionsSeen[evidence.Documents[i].SessionID] = true
			}
		}
		return sessionsSeen
	}
	if got := recalled(channelSession(alice), alice); !got[channelSession(alice)] || got[channelSession(bob)] || got[thread] {
		t.Fatalf("alice recall = %v, want only her own channel session", got)
	}
	if got := recalled(channelSession(bob), bob); !got[channelSession(bob)] || got[channelSession(alice)] {
		t.Fatalf("bob recall = %v, want his own sessions only", got)
	}
	if got := recalled(thread, alice); got[thread] || got[channelSession(bob)] {
		t.Fatalf("alice recall in the shared thread = %v, want none of bob's memory", got)
	}
}

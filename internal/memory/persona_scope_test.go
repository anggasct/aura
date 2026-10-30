package memory

import (
	"slices"
	"testing"
	"time"
)

func personaTurn(session, agentID, text string) *TurnRequestFixture {
	return &TurnRequestFixture{session: session, agentID: agentID, text: text}
}

type TurnRequestFixture struct {
	session string
	agentID string
	text    string
}

func seedPersonaDocument(t *testing.T, service *Service, store *fakeDocumentStore, session, agentID, content string) {
	t.Helper()
	store.setSessionAgent(session, agentID)
	event := &Event{
		ID: "ev-" + session, SessionID: session, Sequence: 1,
		Author: "user", Kind: "adk_event",
		Payload: adkPayload(t, "user", content, false), CreatedAt: time.Now().UTC().Add(-time.Minute),
	}
	if _, _, err := service.ProjectEvent(t.Context(), "owner-1", event); err != nil {
		t.Fatalf("ProjectEvent(): %v", err)
	}
}

func recalledSessions(t *testing.T, provider *Provider, fixture *TurnRequestFixture) []string {
	t.Helper()
	req := turnRequest(fixture.session, fixture.text)
	req.AgentID = fixture.agentID
	evidence, err := provider.RecallTurn(t.Context(), req)
	if err != nil {
		t.Fatalf("RecallTurn(%s): %v", fixture.agentID, err)
	}
	if evidence == nil {
		return nil
	}
	sessions := make([]string, 0, len(evidence.Documents))
	for i := range evidence.Documents {
		sessions = append(sessions, evidence.Documents[i].SessionID)
	}
	slices.Sort(sessions)
	return sessions
}

func TestPersonaScopedRecallIsolatesPersonasForTheSamePrincipal(t *testing.T) {
	store := newFakeDocumentStore()
	service := providerService(t, store, &observationSink{})
	seedPersonaDocument(t, service, store, "solo/discord:home/dm:1", "solo", "the nightly backup finished for solo")
	seedPersonaDocument(t, service, store, "kuma/discord:home/dm:1", "kuma", "the nightly backup finished for kuma")
	seedPersonaDocument(t, service, store, "nara/discord:home/dm:1", "nara", "the nightly backup finished for nara")
	provider, err := NewProvider(service, ProviderConfig{Scopes: staticScopes{
		"solo":   {"solo"},
		"kuma":   {"kuma", "nara"},
		"nara":   {"kuma", "nara"},
		"newbie": {"newbie"},
	}})
	if err != nil {
		t.Fatalf("NewProvider(): %v", err)
	}
	cases := map[string]struct {
		fixture *TurnRequestFixture
		want    []string
	}{
		"persona scope recalls only its own sessions": {
			fixture: personaTurn("solo/discord:home/dm:2", "solo", "nightly backup"),
			want:    []string{"solo/discord:home/dm:1"},
		},
		"shared personas recall each other": {
			fixture: personaTurn("kuma/discord:home/dm:2", "kuma", "nightly backup"),
			want:    []string{"kuma/discord:home/dm:1", "nara/discord:home/dm:1"},
		},
		"persona with no memory of its own recalls nothing": {
			fixture: personaTurn("newbie/discord:home/dm:2", "newbie", "nightly backup"),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := recalledSessions(t, provider, tc.fixture); !slices.Equal(got, tc.want) {
				t.Fatalf("recalled sessions = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRecallFailsClosedForUnregisteredPersona(t *testing.T) {
	store := newFakeDocumentStore()
	service := providerService(t, store, &observationSink{})
	seedPersonaDocument(t, service, store, "main/discord:home/dm:1", "main", "the nightly backup finished")
	provider, err := NewProvider(service, ProviderConfig{Scopes: testScopes})
	if err != nil {
		t.Fatalf("NewProvider(): %v", err)
	}
	req := turnRequest("ghost/discord:home/dm:1", "nightly backup")
	req.AgentID = "ghost"
	evidence, err := provider.RecallTurn(t.Context(), req)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeQueryInvalid || evidence != nil {
		t.Fatalf("RecallTurn = %+v, %v; want %s and no evidence", evidence, err, ErrorCodeQueryInvalid)
	}
	if _, err := service.Recall(t.Context(), &RecallRequest{OwnerID: "owner-1", SessionID: "main/discord:home/dm:1", Query: "nightly backup"}); err == nil {
		t.Fatal("recall without readable personas searched owner-wide")
	}
	if _, err := NewProvider(service, ProviderConfig{}); err == nil {
		t.Fatal("provider without recall scopes accepted")
	}
}

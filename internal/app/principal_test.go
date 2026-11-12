package app

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

func TestWebhookTurnsAreTrustedClassWithUntrustedInput(t *testing.T) {
	req := webhookTurnRequest("webhook:primary", "turn-1", webhookTestEvent())
	if req.PrincipalID != "webhook:primary" || req.PrincipalClass != principal.ClassTrusted || req.Origin != turn.OriginWebhook {
		t.Fatalf("webhook turn = %q/%q/%q, want webhook:primary/trusted/webhook", req.PrincipalID, req.PrincipalClass, req.Origin)
	}
	if !req.Tainted() {
		t.Fatal("a webhook turn is not tainted")
	}
}

func TestCronSessionsAreOwnerKeyed(t *testing.T) {
	db := discordTestDB(t)
	sessions := store.NewSessionService(db)
	if err := ensureCronSession(t.Context(), sessions, "cron:job-1"); err != nil {
		t.Fatalf("ensureCronSession: %v", err)
	}
	got, err := sessions.Get(t.Context(), "cron:job-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OwnerID != principal.OwnerID {
		t.Fatalf("cron session owner = %q, want %q", got.OwnerID, principal.OwnerID)
	}
}

func TestChildTurnsInheritParentClass(t *testing.T) {
	cases := map[string]struct {
		owner string
		class principal.Class
	}{
		"owner parent":   {owner: principal.OwnerID, class: principal.ClassOwner},
		"trusted parent": {owner: "discord:111", class: principal.ClassTrusted},
		"legacy parent":  {owner: "cron", class: principal.ClassTrusted},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			req := childTurnRequest("sess-child-1", "ch-1", "work", time.Now().Add(time.Minute), &childProjection{ownerID: tc.owner})
			if req.PrincipalID != tc.owner || req.PrincipalClass != tc.class {
				t.Fatalf("child principal = %q/%q, want %q/%q", req.PrincipalID, req.PrincipalClass, tc.owner, tc.class)
			}
		})
	}
	if req := childTurnRequest("sess-child-1", "", "work", time.Time{}, &childProjection{ownerID: principal.OwnerID}); req.TurnID != "sess-child-1" || req.IdempotencyKey != "sess-child-1" {
		t.Fatalf("anonymous child turn = %+v, want session-keyed ids", req)
	}
}

func TestChildTurnsCarryUntrustedInput(t *testing.T) {
	for name, owner := range map[string]string{"owner parent": principal.OwnerID, "trusted parent": "discord:111"} {
		t.Run(name, func(t *testing.T) {
			req := childTurnRequest("sess-child-1", "ch-1", "work", time.Now().Add(time.Minute), &childProjection{ownerID: owner})
			if req.Origin != turn.OriginChild || !req.Tainted() {
				t.Fatalf("child turn origin = %q tainted = %v, want child and tainted", req.Origin, req.Tainted())
			}
		})
	}
}

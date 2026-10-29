package cli

import (
	"testing"
	"time"

	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/store"
)

func TestWebhookTurnsAreTrusted(t *testing.T) {
	req := webhookTurnRequest("webhook:primary", "turn-1", webhookTestEvent())
	if req.PrincipalID != "webhook:primary" || req.PrincipalClass != principal.ClassTrusted {
		t.Fatalf("webhook principal = %q/%q, want webhook:primary/trusted", req.PrincipalID, req.PrincipalClass)
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

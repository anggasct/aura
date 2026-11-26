package app

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
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
	sessions := turnstore.NewSessionService(db)
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

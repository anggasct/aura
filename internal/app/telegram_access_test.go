package app

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/store"
)

func writeTelegramAccessConfig(t *testing.T) string {
	t.Helper()
	dataRoot := t.TempDir()
	path := dataRoot + "/config.yaml"
	content := `version: 1
storage:
  path: ` + dataRoot + `
tools:
  workspace: ` + dataRoot + `
owner:
  identities: ["telegram:777"]
channels:
  telegram:
    enabled: true
    accounts:
      - name: pocket
        bot_token_ref: env://AURA_TEST_POCKET_TOKEN
        persona: main
        allowed_user_ids: ["777", "5"]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	seedConfiguredStorage(t, path)
	return path
}

func TestPairingApproveAndRevokeForTelegram(t *testing.T) {
	cfg := writeTelegramAccessConfig(t)
	db := openAccessDB(t, cfg)
	service := newPairingService(db, nil)
	issued, err := service.Request(t.Context(), "telegram:1", "pocket")
	if err != nil || issued.Outcome != access.PairingOutcomeIssued {
		t.Fatalf("Request() = %+v, %v", issued, err)
	}
	if _, err := runAccessCommand(t, newPairingCmd, cfg, "approve", "discord", issued.Code); err == nil || !strings.Contains(err.Error(), "pairing_invalid") {
		t.Fatalf("approving a telegram code as discord err = %v, want pairing_invalid", err)
	}
	if granted, _ := service.Granted(t.Context(), "telegram:1", "pocket"); granted {
		t.Fatal("a cross-channel approval granted access")
	}
	if out, err := runAccessCommand(t, newPairingCmd, cfg, "approve", "telegram", strings.ToLower(issued.Code)); err != nil || strings.TrimSpace(out) != "granted telegram:1 on pocket" {
		t.Fatalf("approve = %q, %v", out, err)
	}
	if granted, _ := service.Granted(t.Context(), "telegram:1", "pocket"); !granted {
		t.Fatal("approval did not grant trusted access")
	}
	if out, err := runAccessCommand(t, newPairingCmd, cfg, "revoke", "telegram", "1"); err != nil || strings.TrimSpace(out) != "revoked telegram:1 on pocket" {
		t.Fatalf("revoke = %q, %v", out, err)
	}
	if _, err := runAccessCommand(t, newPairingCmd, cfg, "revoke", "telegram", "-4"); err == nil {
		t.Fatal("negative telegram id accepted")
	}
	if _, err := runAccessCommand(t, newPairingCmd, cfg, "revoke", "slack", "1"); err == nil {
		t.Fatal("unknown channel accepted")
	}
}

func TestPairingWrongTelegramCodesLockOnlyTelegramApprovals(t *testing.T) {
	cfg := writeTelegramAccessConfig(t)
	db := openAccessDB(t, cfg)
	service := newPairingService(db, nil)
	telegramCode, err := service.Request(t.Context(), "telegram:1", "pocket")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	discordCode, err := service.Request(t.Context(), "discord:2", "office")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	for range pairingLockoutFailures {
		if _, err := runAccessCommand(t, newPairingCmd, cfg, "approve", "telegram", "ZZZZZZZZ"); err == nil || !strings.Contains(err.Error(), "pairing_invalid") {
			t.Fatalf("wrong code err = %v", err)
		}
	}
	if _, err := runAccessCommand(t, newPairingCmd, cfg, "approve", "telegram", telegramCode.Code); err == nil || !strings.Contains(err.Error(), "pairing_locked") {
		t.Fatalf("approve during lockout err = %v", err)
	}
	if out, err := runAccessCommand(t, newPairingCmd, cfg, "approve", "discord", discordCode.Code); err != nil || strings.TrimSpace(out) != "granted discord:2 on office" {
		t.Fatalf("discord approve during a telegram lockout = %q, %v", out, err)
	}
}

func TestPrincipalListIncludesTelegramPrincipals(t *testing.T) {
	cfg := writeTelegramAccessConfig(t)
	db := openAccessDB(t, cfg)
	service := newPairingService(db, nil)
	issued, err := service.Request(t.Context(), "telegram:1", "pocket")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if _, _, err := store.NewPairingStore(db).Approve(t.Context(), principal.ChannelTelegram, pairingCodeDigest(issued.Code), principal.OwnerID, time.Now().UTC()); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	now := time.Now().UTC()
	if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: "main/telegram:pocket/chat:9", OwnerID: "telegram:9", CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	out, err := runAccessCommand(t, newPrincipalCmd, cfg, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"telegram:1\ttrusted\tgrant:pocket\t0\t", "telegram:5\ttrusted\tallowlist:pocket\t0\t-", "telegram:9\tpublic\t-\t1\t"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output %q missing %q", out, want)
		}
	}
	if strings.Contains(out, "777") || strings.Contains(out, principal.OwnerID) {
		t.Fatalf("list output shows the owner: %q", out)
	}
	if out, err := runAccessCommand(t, newPrincipalCmd, cfg, "show", "telegram:5"); err != nil || !strings.Contains(out, "access: allowlist:pocket") {
		t.Fatalf("show = %q, %v", out, err)
	}
}

func TestTelegramAccessSettingsShareTheDiscordGates(t *testing.T) {
	cfg := enabledTelegramConfig()
	db := discordTestDB(t)
	settings := telegramAccessSettings(&cfg, db, nil, nil)
	if settings.Pairing == nil || settings.Budgets == nil || settings.Erasure == nil || settings.Handoffs == nil || settings.Evals == nil {
		t.Fatalf("telegram access settings are missing gates: %+v", settings)
	}
	bare := telegramAccessSettings(&cfg, nil, nil, nil)
	if bare.Budgets != nil || bare.Erasure != nil || bare.Handoffs != nil || bare.Evals != nil {
		t.Fatalf("settings without a database carry gates: %+v", bare)
	}
}

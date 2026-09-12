package cli

import (
	"database/sql"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
)

func discordTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.OpenDB(t.Context(), t.TempDir()+"/aura.db")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func enabledDiscordConfig() config.Config {
	defaults := config.Default()
	defaults.Channels.Discord.Enabled = true
	defaults.Channels.Discord.AllowedUserIDs = []string{"111"}
	return defaults
}

func TestBuildChannelAdapters_DisabledByDefault(t *testing.T) {
	db := discordTestDB(t)
	defaults := config.Default()
	adapters, checks, _, err := buildChannelAdapters(&defaults, db, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("buildChannelAdapters(): %v", err)
	}
	if len(adapters) != 0 || len(checks) != 0 {
		t.Errorf("adapters = %d, checks = %d; want none when disabled", len(adapters), len(checks))
	}
}

func TestBuildChannelAdapters_EnabledBuildsAdapterAndCheck(t *testing.T) {
	db := discordTestDB(t)
	cfg := enabledDiscordConfig()
	adapters, checks, decider, err := buildChannelAdapters(&cfg, db, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("buildChannelAdapters(): %v", err)
	}
	if decider == nil {
		t.Error("decider is nil for enabled adapter")
	}
	if len(adapters) != 1 {
		t.Fatalf("adapters = %d, want 1", len(adapters))
	}
	if len(checks) != 1 || checks[0].ID != "discord" || checks[0].Checker == nil {
		t.Fatalf("checks = %+v, want one discord check", checks)
	}
	if findings := checks[0].Checker.Check(t.Context()); len(findings) != 0 {
		t.Errorf("fresh adapter findings = %+v, want none", findings)
	}
}

func TestBuildChannelAdapters_RejectsBadTokenRef(t *testing.T) {
	db := discordTestDB(t)
	cfg := enabledDiscordConfig()
	cfg.Channels.Discord.BotTokenRef = "not-a-reference"
	if _, _, _, err := buildChannelAdapters(&cfg, db, t.TempDir(), nil); err == nil {
		t.Fatal("bad token reference accepted")
	}
}

func TestDiscordResumeStore_RoundTrip(t *testing.T) {
	db := discordTestDB(t)
	wrapper := &discordResumeStore{store: store.NewChannelResumeStore(db), instance: "owner"}
	ctx := t.Context()

	now := time.Now().UTC()
	if err := wrapper.Save(ctx, &discord.ResumeCursor{
		SessionID:    "session-a",
		Sequence:     7,
		ConfigDigest: "digest-a",
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	cursor, found, err := wrapper.Load(ctx)
	if err != nil || !found {
		t.Fatalf("Load(): %v, found = %v", err, found)
	}
	if cursor.SessionID != "session-a" || cursor.Sequence != 7 || cursor.ConfigDigest != "digest-a" {
		t.Errorf("cursor = %+v", cursor)
	}
	if err := wrapper.Delete(ctx); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	if _, found, err := wrapper.Load(ctx); err != nil || found {
		t.Errorf("Load() after delete: %v, found = %v", err, found)
	}
}

func TestDiscordSessionEnsurer_CreatesOnce(t *testing.T) {
	db := discordTestDB(t)
	ensurer := &discordSessionEnsurer{sessions: store.NewSessionService(db)}
	ctx := t.Context()

	if err := ensurer.EnsureSession(ctx, "dm:111", "111"); err != nil {
		t.Fatalf("EnsureSession(): %v", err)
	}
	session, err := store.NewSessionService(db).Get(ctx, "dm:111")
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if session.OwnerID != "111" {
		t.Errorf("owner = %q, want principal", session.OwnerID)
	}
	if err := ensurer.EnsureSession(ctx, "dm:111", "111"); err != nil {
		t.Fatalf("second EnsureSession(): %v", err)
	}
}

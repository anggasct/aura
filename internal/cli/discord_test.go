package cli

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
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
	defaults.Channels.Discord.Accounts = []config.DiscordAccount{{
		Name:           "owner",
		BotTokenRef:    "env://AURA_DISCORD_BOT_TOKEN",
		Persona:        auraagent.DefaultID,
		AllowedUserIDs: []string{"111"},
		AcceptDMs:      true,
	}}
	return defaults
}

func testRouter(t *testing.T, cfg *config.Config) *auraagent.Router {
	t.Helper()
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		t.Fatalf("buildAgentRegistry(): %v", err)
	}
	router, err := auraagent.NewRouter(registry, &cfg.Channels.Discord, cfg.Bindings)
	if err != nil {
		t.Fatalf("NewRouter(): %v", err)
	}
	return router
}

func TestBuildChannelAdapters_DisabledByDefault(t *testing.T) {
	db := discordTestDB(t)
	defaults := config.Default()
	adapters, checks, _, err := buildChannelAdapters(&defaults, testRouter(t, &defaults), db, t.TempDir(), nil, nil)
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
	adapters, checks, decider, err := buildChannelAdapters(&cfg, testRouter(t, &cfg), db, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("buildChannelAdapters(): %v", err)
	}
	if decider == nil {
		t.Error("decider is nil for enabled adapter")
	}
	if len(adapters) != 1 {
		t.Fatalf("adapters = %d, want 1", len(adapters))
	}
	if len(checks) != 2 || checks[0].ID != "discord" || checks[0].Checker == nil || checks[1].ID != "principal" {
		t.Fatalf("checks = %+v, want discord and principal checks", checks)
	}
	if findings := checks[0].Checker.Check(t.Context()); len(findings) != 0 {
		t.Errorf("fresh adapter findings = %+v, want none", findings)
	}
}

func TestBuildChannelAdapters_ReportsUnmappedOwnerIdentity(t *testing.T) {
	cases := map[string]struct {
		identities []string
		finding    bool
	}{
		"no identities":        {finding: true},
		"discord identity set": {identities: []string{"discord:123456789012345678"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := discordTestDB(t)
			cfg := enabledDiscordConfig()
			cfg.Owner = &config.Owner{Identities: tc.identities}
			_, checks, _, err := buildChannelAdapters(&cfg, testRouter(t, &cfg), db, t.TempDir(), nil, nil)
			if err != nil {
				t.Fatalf("buildChannelAdapters(): %v", err)
			}
			findings := checks[1].Checker.Check(t.Context())
			if got := len(findings) == 1 && findings[0].Code == "principal_identities_unmapped" && findings[0].Severity == health.SeverityWarning; got != tc.finding {
				t.Fatalf("findings = %+v, want unmapped finding = %v", findings, tc.finding)
			}
			for _, finding := range findings {
				for _, id := range tc.identities {
					if strings.Contains(finding.Detail+finding.Remediation, id) {
						t.Errorf("finding leaks identity: %+v", finding)
					}
				}
			}
		})
	}
}

func TestBuildSecretVaultSkipsEmptyValues(t *testing.T) {
	t.Setenv("AURA_TEST_EMPTY_WEBHOOK_SECRET", "")
	t.Setenv("AURA_TEST_EMPTY_MODEL_KEY", "")
	cfg := config.Default()
	cfg.Webhook.Keys = []config.WebhookKey{{ID: "probe", SecretEnv: "AURA_TEST_EMPTY_WEBHOOK_SECRET"}}
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"probe": {Model: "probe", Provider: "probe", APIKeyEnv: "AURA_TEST_EMPTY_MODEL_KEY"},
	}
	vault := buildSecretVault(&cfg)
	rendered := "Looking through notes"
	if got := vault.Redact(rendered); got != rendered {
		t.Fatalf("Redact() = %q, want unchanged status text", got)
	}
	if vault.Contains("") {
		t.Fatal("vault treats an empty value as a secret match")
	}
}

func TestBuildChannelAdapters_RejectsBadTokenRef(t *testing.T) {
	db := discordTestDB(t)
	cfg := enabledDiscordConfig()
	cfg.Channels.Discord.Accounts[0].BotTokenRef = "not-a-reference"
	if _, _, _, err := buildChannelAdapters(&cfg, testRouter(t, &cfg), db, t.TempDir(), nil, nil); err == nil {
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

	if err := ensurer.EnsureSession(ctx, "kuma/discord:home/dm:111", "111", "kuma"); err != nil {
		t.Fatalf("EnsureSession(): %v", err)
	}
	session, err := store.NewSessionService(db).Get(ctx, "kuma/discord:home/dm:111")
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if session.OwnerID != "111" || session.AgentID != "kuma" {
		t.Errorf("owner, persona = %q, %q; want principal and kuma", session.OwnerID, session.AgentID)
	}
	if err := ensurer.EnsureSession(ctx, "kuma/discord:home/dm:111", "111", "kuma"); err != nil {
		t.Fatalf("second EnsureSession(): %v", err)
	}
}

func TestBuildChannelAdapters_OneAdapterPerAccount(t *testing.T) {
	db := discordTestDB(t)
	cfg := enabledDiscordConfig()
	cfg.Channels.Discord.Accounts = append(cfg.Channels.Discord.Accounts, config.DiscordAccount{
		Name:           "kuma",
		BotTokenRef:    "env://AURA_DISCORD_KUMA_TOKEN",
		Persona:        "engineer",
		AllowedUserIDs: []string{"111"},
	})
	ports, checks, decider, err := buildChannelAdapters(&cfg, testRouter(t, &cfg), db, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("buildChannelAdapters(): %v", err)
	}
	if len(ports) != 2 || decider == nil {
		t.Fatalf("ports = %d, decider nil = %v; want two adapters and a decider", len(ports), decider == nil)
	}
	names := make([]string, 0, len(ports))
	for _, port := range ports {
		adapter, ok := port.(*discord.Adapter)
		if !ok {
			t.Fatalf("port %T is not a discord adapter", port)
		}
		names = append(names, adapter.ChannelName())
	}
	if names[0] != "discord:owner" || names[1] != "discord:kuma" {
		t.Errorf("adapter names = %v, want one per account in config order", names)
	}
	if len(checks) != 2 || checks[0].ID != "discord" {
		t.Fatalf("checks = %+v, want one aggregated discord check", checks)
	}
}

func TestBuildChannelAdapters_RequiresRouter(t *testing.T) {
	db := discordTestDB(t)
	cfg := enabledDiscordConfig()
	if _, _, _, err := buildChannelAdapters(&cfg, nil, db, t.TempDir(), nil, nil); err == nil {
		t.Fatal("enabled discord without a persona router accepted")
	}
}

func TestDiscordSessionEnsurer_ReportsExistence(t *testing.T) {
	db := discordTestDB(t)
	ensurer := &discordSessionEnsurer{sessions: store.NewSessionService(db)}
	ctx := t.Context()
	exists, err := ensurer.SessionExists(ctx, "kuma/discord:home/thread:9")
	if err != nil || exists {
		t.Fatalf("SessionExists() before create = %v, %v; want false", exists, err)
	}
	if err := ensurer.EnsureSession(ctx, "kuma/discord:home/thread:9", "111", "kuma"); err != nil {
		t.Fatalf("EnsureSession(): %v", err)
	}
	exists, err = ensurer.SessionExists(ctx, "kuma/discord:home/thread:9")
	if err != nil || !exists {
		t.Fatalf("SessionExists() after create = %v, %v; want true", exists, err)
	}
}

func TestBuildSecretVaultRedactsEveryAccountToken(t *testing.T) {
	t.Setenv("AURA_TEST_HOME_TOKEN", "home-token-value-123")
	t.Setenv("AURA_TEST_KUMA_TOKEN", "kuma-token-value-456")
	cfg := config.Default()
	cfg.Channels.Discord.Accounts = []config.DiscordAccount{
		{Name: "home", BotTokenRef: "env://AURA_TEST_HOME_TOKEN"},
		{Name: "kuma", BotTokenRef: "env://AURA_TEST_KUMA_TOKEN"},
	}
	vault := buildSecretVault(&cfg)
	for _, token := range []string{"home-token-value-123", "kuma-token-value-456"} {
		if strings.Contains(vault.Redact("leak "+token), token) {
			t.Errorf("token for an account is not redacted")
		}
	}
}

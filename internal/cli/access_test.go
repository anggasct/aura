package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/store"
)

func runGrantsCommand(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	cmd := newGrantsCmd(&globalFlags{configPath: cfg})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func accessEventCount(t *testing.T, cfg, action string) int {
	t.Helper()
	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM runtime_event WHERE kind = ? AND json_extract(payload_json, '$.action') = ?`, store.AccessOwnerActionEventKind, action).Scan(&count); err != nil {
		t.Fatalf("count owner actions: %v", err)
	}
	return count
}

func recordTestGrant(t *testing.T, cfg string) string {
	t.Helper()
	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := newStandingGrantRecorder(db).RecordStandingGrant(t.Context(), &discord.StandingGrantRequest{
		PrincipalID: "discord:111", AgentID: "kantor", ToolName: "write_file", OwnerID: "discord:777", TTL: 168 * time.Hour,
	}); err != nil {
		t.Fatalf("RecordStandingGrant: %v", err)
	}
	grants, err := store.NewStandingGrantStore(db).List(t.Context(), "", time.Now().UTC())
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %+v, %v", grants, err)
	}
	return grants[0].ID
}

func TestGrantsListAndRevoke(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	if out, err := runGrantsCommand(t, cfg, "list"); err != nil || strings.TrimSpace(out) != "no standing grants" {
		t.Fatalf("empty list = %q, %v", out, err)
	}
	id := recordTestGrant(t, cfg)
	if accessEventCount(t, cfg, accessActionStandingGrant) != 1 {
		t.Fatal("standing grant left no owner-action event")
	}
	out, err := runGrantsCommand(t, cfg, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "ID\tPRINCIPAL\tPERSONA\tTOOL\tEXPIRES") || !strings.Contains(out, id+"\tdiscord:111\tkantor\twrite_file\t") {
		t.Fatalf("list output = %q", out)
	}
	if out, _ := runGrantsCommand(t, cfg, "list", "--principal", "discord:222"); strings.TrimSpace(out) != "no standing grants" {
		t.Fatalf("filtered list = %q", out)
	}
	if out, err := runGrantsCommand(t, cfg, "revoke", id); err != nil || strings.TrimSpace(out) != "revoked "+id {
		t.Fatalf("revoke = %q, %v", out, err)
	}
	if accessEventCount(t, cfg, accessActionStandingGrantRevoke) != 1 {
		t.Fatal("revoke left no owner-action event")
	}
	if out, _ := runGrantsCommand(t, cfg, "list"); strings.TrimSpace(out) != "no standing grants" {
		t.Fatalf("list after revoke = %q", out)
	}
	_, err = runGrantsCommand(t, cfg, "revoke", id)
	if err == nil || !strings.Contains(err.Error(), "standing_grant_not_found") {
		t.Fatalf("second revoke err = %v", err)
	}
	if _, err := runGrantsCommand(t, cfg, "revoke"); err == nil {
		t.Fatal("revoke without an id succeeded")
	}
}

func TestAccessPolicyHonorsPrincipalToolsAndExpiry(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	registry, err := auraagent.Build([]config.AgentDefinition{{
		ID: "kantor", Description: "d", Instructions: "i", Audience: config.AudienceTrusted,
		Tools: []string{"read_file", "write_file", "exec"}, PrincipalTools: []string{"write_file"},
	}}, []string{"read_file", "write_file", "exec"}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	policy := newAccessPolicy(registry, db, nil)
	if ceiling, known := policy.ToolCeiling("kantor", principal.ClassTrusted); !known || len(ceiling) != 1 || ceiling[0] != "write_file" {
		t.Fatalf("ToolCeiling = %v, %v", ceiling, known)
	}
	if _, known := policy.ToolCeiling("missing", principal.ClassTrusted); known {
		t.Fatal("unknown persona reported a ceiling")
	}
	now := time.Now().UTC()
	grants := store.NewStandingGrantStore(db)
	for _, tool := range []string{"write_file", "exec"} {
		if _, err := grants.Grant(t.Context(), &store.StandingGrant{PrincipalID: "discord:111", AgentID: "kantor", ToolName: tool, GrantedBy: "discord:777", GrantedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatalf("Grant %s: %v", tool, err)
		}
	}
	if active, err := policy.StandingGrant(t.Context(), "discord:111", "kantor", "write_file"); err != nil || !active {
		t.Fatalf("write_file standing = %v, %v; want active", active, err)
	}
	if active, _ := policy.StandingGrant(t.Context(), "discord:111", "kantor", "exec"); active {
		t.Fatal("standing grant for a tool outside principal_tools still applied")
	}
	policy.now = func() time.Time { return now.Add(2 * time.Hour) }
	if active, _ := policy.StandingGrant(t.Context(), "discord:111", "kantor", "write_file"); active {
		t.Fatal("expired standing grant still applied")
	}
}

func TestDiscordAccessSettingsUseTheAccessSection(t *testing.T) {
	cfg := config.Default()
	cfg.Access.ApprovalTimeout = config.Duration(10 * time.Minute)
	settings := discordAccessSettings(&cfg, nil, nil)
	if settings.ApprovalTimeout != 10*time.Minute || settings.StandingGrantTTL != 168*time.Hour || settings.Grants == nil {
		t.Fatalf("settings = %+v", settings)
	}
}

package cli

import (
	"bytes"
	"database/sql"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
)

func runAccessCommand(t *testing.T, build func(*globalFlags) *cobra.Command, cfg string, args ...string) (string, error) {
	t.Helper()
	cmd := build(&globalFlags{configPath: cfg})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func openAccessDB(t *testing.T, cfg string) *sql.DB {
	t.Helper()
	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestPairingCodesAreCrockfordAndNormalize(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{8}$`)
	seen := map[string]bool{}
	for range 64 {
		code, err := newPairingCode()
		if err != nil || !shape.MatchString(code) {
			t.Fatalf("newPairingCode() = %q, %v", code, err)
		}
		seen[code] = true
	}
	if len(seen) < 60 {
		t.Fatalf("pairing codes repeat too often: %d distinct of 64", len(seen))
	}
	if pairingCodeDigest(" ab1o-0ilz ") != pairingCodeDigest("AB10011Z") {
		t.Fatal("equivalent spellings hash differently")
	}
	if pairingCodeDigest("ABCD-EFGH") != pairingCodeDigest("abcdefgh") {
		t.Fatal("case and separators change the digest")
	}
	if pairingCodeDigest("ABCDEFGH") == pairingCodeDigest("ABCDEFGJ") {
		t.Fatal("distinct codes share a digest")
	}
}

func TestPairingLockedUntil(t *testing.T) {
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	spaced := func(gap time.Duration, n int) []time.Time {
		out := make([]time.Time, n)
		for i := range out {
			out[i] = base.Add(time.Duration(i) * gap)
		}
		return out
	}
	cases := map[string]struct {
		failures []time.Time
		now      time.Time
		locked   bool
		until    time.Time
	}{
		"four failures":                 {spaced(time.Minute, 4), base.Add(5 * time.Minute), false, time.Time{}},
		"five within the hour":          {spaced(time.Minute, 5), base.Add(5 * time.Minute), true, base.Add(4*time.Minute + time.Hour)},
		"lock ends after an hour":       {spaced(time.Minute, 5), base.Add(4*time.Minute + time.Hour), false, base.Add(4*time.Minute + time.Hour)},
		"five spread beyond an hour":    {spaced(20*time.Minute, 5), base.Add(81 * time.Minute), false, time.Time{}},
		"five spanning exactly an hour": {spaced(15*time.Minute, 5), base.Add(61 * time.Minute), true, base.Add(2 * time.Hour)},
		"five spanning just over an hour": {
			append(spaced(15*time.Minute, 4), base.Add(time.Hour+time.Nanosecond)), base.Add(61 * time.Minute), false, time.Time{},
		},
		"lock holds until its last instant": {spaced(time.Minute, 5), base.Add(4*time.Minute + time.Hour - time.Nanosecond), true, base.Add(4*time.Minute + time.Hour)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			until, locked := pairingLockedUntil(tc.failures, tc.now)
			if locked != tc.locked || !until.Equal(tc.until) {
				t.Fatalf("pairingLockedUntil() = %v, %v; want %v, %v", until, locked, tc.until, tc.locked)
			}
		})
	}
}

func TestPairingLockLooksBackFarEnoughForTheOldestRun(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	db := openAccessDB(t, cfg)
	now := time.Now().UTC().Truncate(time.Second)
	first := now.Add(-2*time.Hour + time.Minute)
	for i := range pairingLockoutFailures {
		at := first.Add(time.Duration(i) * 15 * time.Minute)
		if err := recordAccessAction(t.Context(), db, &accessAction{Action: store.AccessActionPairingInvalid, Channel: "discord"}, "discord", at); err != nil {
			t.Fatalf("recordAccessAction: %v", err)
		}
	}
	until, locked, err := pairingLock(t.Context(), store.NewPairingStore(db), "discord", now)
	if err != nil || !locked || !until.Equal(first.Add(2*time.Hour)) {
		t.Fatalf("pairingLock() = %v, %v, %v; want locked until %v", until, locked, err, first.Add(2*time.Hour))
	}
	if _, locked, _ := pairingLock(t.Context(), store.NewPairingStore(db), "telegram", now); locked {
		t.Fatal("discord wrong codes locked another channel")
	}
}

func TestPairingServiceIssuesPendingAndLocked(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	db := openAccessDB(t, cfg)
	service := newPairingService(db, nil)
	now := time.Now().UTC()
	service.now = func() time.Time { return now }
	issued, err := service.Request(t.Context(), "discord:1", "office")
	if err != nil || issued.Outcome != discord.PairingOutcomeIssued || len(issued.Code) != pairingCodeLength {
		t.Fatalf("first Request() = %+v, %v", issued, err)
	}
	var stored string
	if err := db.QueryRowContext(t.Context(), `SELECT code_sha256 FROM pairing_request`).Scan(&stored); err != nil || stored != pairingCodeDigest(issued.Code) || strings.Contains(stored, issued.Code) {
		t.Fatalf("stored code = %q, %v; want only the digest", stored, err)
	}
	if again, _ := service.Request(t.Context(), "discord:1", "office"); again.Outcome != discord.PairingOutcomePending || again.Code != "" {
		t.Fatalf("repeat Request() = %+v, want pending without a code", again)
	}
	for i := range pairingLockoutFailures {
		if err := recordAccessAction(t.Context(), db, &accessAction{Action: store.AccessActionPairingInvalid, Channel: "discord"}, "discord", now.Add(-time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("record failure: %v", err)
		}
	}
	if locked, _ := service.Request(t.Context(), "discord:2", "office"); locked.Outcome != discord.PairingOutcomeLocked || locked.Code != "" {
		t.Fatalf("locked Request() = %+v", locked)
	}
	if granted, err := service.Granted(t.Context(), "discord:1", "office"); err != nil || granted {
		t.Fatalf("Granted() before approval = %v, %v", granted, err)
	}
}

func TestPairingApproveRevokeListAndClear(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	db := openAccessDB(t, cfg)
	service := newPairingService(db, nil)
	issued, err := service.Request(t.Context(), "discord:1", "office")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	out, err := runAccessCommand(t, newPairingCmd, cfg, "list")
	if err != nil || !strings.Contains(out, "ACCOUNT\tPRINCIPAL\tEXPIRES") || !strings.Contains(out, "office\tdiscord:1\t") || strings.Contains(out, issued.Code) {
		t.Fatalf("list = %q, %v", out, err)
	}
	if _, err := runAccessCommand(t, newPairingCmd, cfg, "approve", "telegram", issued.Code); err == nil {
		t.Fatal("unknown channel accepted")
	}
	if out, err := runAccessCommand(t, newPairingCmd, cfg, "approve", "discord", strings.ToLower(issued.Code)); err != nil || strings.TrimSpace(out) != "granted discord:1 on office" {
		t.Fatalf("approve = %q, %v", out, err)
	}
	if granted, _ := service.Granted(t.Context(), "discord:1", "office"); !granted {
		t.Fatal("approval did not grant trusted access")
	}
	if accessEventCount(t, cfg, accessActionPrincipalGrant) != 1 {
		t.Fatal("approval left no owner-action event")
	}
	if _, err := runAccessCommand(t, newPairingCmd, cfg, "approve", "discord", issued.Code); err == nil || !strings.Contains(err.Error(), "pairing_invalid") {
		t.Fatalf("reused code err = %v", err)
	}
	if out, err := runAccessCommand(t, newPairingCmd, cfg, "revoke", "discord", "1"); err != nil || strings.TrimSpace(out) != "revoked discord:1 on office" {
		t.Fatalf("revoke = %q, %v", out, err)
	}
	if granted, _ := service.Granted(t.Context(), "discord:1", "office"); granted {
		t.Fatal("revoke left trusted access")
	}
	if _, err := runAccessCommand(t, newPairingCmd, cfg, "revoke", "discord", "discord:1"); err == nil || !strings.Contains(err.Error(), "grant_not_found") {
		t.Fatalf("second revoke err = %v", err)
	}
	if _, err := runAccessCommand(t, newPairingCmd, cfg, "revoke", "discord", "not-a-snowflake"); err == nil {
		t.Fatal("invalid principal accepted")
	}
	if _, err := service.Request(t.Context(), "discord:2", "office"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if out, err := runAccessCommand(t, newPairingCmd, cfg, "clear-pending"); err != nil || strings.TrimSpace(out) != "cleared 1 pending pairing requests" {
		t.Fatalf("clear-pending = %q, %v", out, err)
	}
	if out, _ := runAccessCommand(t, newPairingCmd, cfg, "list"); strings.TrimSpace(out) != "no pending pairing requests" {
		t.Fatalf("list after clear = %q", out)
	}
}

func TestPairingWrongCodesLockApprovals(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	db := openAccessDB(t, cfg)
	issued, err := newPairingService(db, nil).Request(t.Context(), "discord:1", "office")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	for i := range pairingLockoutFailures {
		_, err := runAccessCommand(t, newPairingCmd, cfg, "approve", "discord", "ZZZZZZZZ")
		if err == nil || !strings.Contains(err.Error(), "pairing_invalid") {
			t.Fatalf("wrong code %d err = %v", i, err)
		}
	}
	if accessEventCount(t, cfg, store.AccessActionPairingInvalid) != pairingLockoutFailures {
		t.Fatal("wrong codes were not recorded")
	}
	_, err = runAccessCommand(t, newPairingCmd, cfg, "approve", "discord", issued.Code)
	if err == nil || !strings.Contains(err.Error(), "pairing_locked") {
		t.Fatalf("approve during lockout err = %v", err)
	}
	if accessEventCount(t, cfg, store.AccessActionPairingInvalid) != pairingLockoutFailures {
		t.Fatal("attempts during the lockout were counted")
	}
	if granted, _ := newPairingService(db, nil).Granted(t.Context(), "discord:1", "office"); granted {
		t.Fatal("locked approval granted access")
	}
}

func writePrincipalCLIConfig(t *testing.T) string {
	t.Helper()
	dataRoot := t.TempDir()
	path := dataRoot + "/config.yaml"
	content := `version: 1
storage:
  path: ` + dataRoot + `
tools:
  workspace: ` + dataRoot + `
owner:
  identities: ["discord:777"]
channels:
  discord:
    enabled: true
    accounts:
      - name: office
        bot_token_ref: env://AURA_TEST_OFFICE_TOKEN
        persona: main
        allowed_user_ids: ["777", "5"]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	seedConfiguredStorage(t, path)
	return path
}

func TestPrincipalListAndShow(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	db := openAccessDB(t, cfg)
	service := newPairingService(db, nil)
	issued, err := service.Request(t.Context(), "discord:1", "office")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if _, _, err := store.NewPairingStore(db).Approve(t.Context(), "discord", pairingCodeDigest(issued.Code), "local-owner", time.Now().UTC()); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	now := time.Now().UTC()
	sessions := store.NewSessionService(db)
	for id, owner := range map[string]string{"main/discord:office/dm:1": "discord:1", "main/discord:office/dm:9": "discord:9", "main/discord:office/dm:777": "local-owner"} {
		if err := sessions.Create(t.Context(), &store.Session{ID: id, OwnerID: owner, CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
			t.Fatalf("Create session: %v", err)
		}
	}
	if _, err := service.Request(t.Context(), "discord:9", "office"); err != nil {
		t.Fatalf("Request: %v", err)
	}
	out, err := runAccessCommand(t, newPrincipalCmd, cfg, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"discord:1\ttrusted\tgrant:office\t1\t", "discord:5\ttrusted\tallowlist:office\t0\t-", "discord:9\tpublic\t-\t1\t"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list output %q missing %q", out, want)
		}
	}
	if strings.Contains(out, "777") || strings.Contains(out, "local-owner") {
		t.Fatalf("list output shows the owner: %q", out)
	}
	if out, _ := runAccessCommand(t, newPrincipalCmd, cfg, "list", "--class", "public"); strings.Contains(out, "discord:1\t") || !strings.Contains(out, "discord:9\t") {
		t.Fatalf("public list = %q", out)
	}
	if _, err := runAccessCommand(t, newPrincipalCmd, cfg, "list", "--class", "owner"); err == nil {
		t.Fatal("owner class accepted")
	}
	out, err = runAccessCommand(t, newPrincipalCmd, cfg, "show", "discord:9")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{"principal: discord:9", "class: public", "access: -", "standing_grants: 0", "pending_pairing: 1", "sessions: 1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output %q missing %q", out, want)
		}
	}
	if _, err := runAccessCommand(t, newPrincipalCmd, cfg, "show", "discord:404"); err == nil || !strings.Contains(err.Error(), "principal_not_found") {
		t.Fatalf("unknown show err = %v", err)
	}
}

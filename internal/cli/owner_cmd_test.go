package cli

import (
	"bytes"
	"os"
	"os/user"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/store"
)

const (
	testOwnerDiscordID   = "123456789012345678"
	testTrustedDiscordID = "222222222222222222"
)

func writeOwnerCLIConfig(t *testing.T) string {
	t.Helper()
	path := writeProfileCLIConfig(t)
	extra := `owner:
  name: Kuma
  identities:
    - discord:` + testOwnerDiscordID + `
channels:
  discord:
    allowed_user_ids: ["` + testOwnerDiscordID + `", "` + testTrustedDiscordID + `"]
`
	loaded, err := readFileForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileForTest(path, loaded+extra); err != nil {
		t.Fatal(err)
	}
	return path
}

func runOwnerCommand(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	cmd := newOwnerCmd(&globalFlags{configPath: cfg})
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestOwnerWhoamiMasksIdentities(t *testing.T) {
	out, err := runOwnerCommand(t, writeOwnerCLIConfig(t), "whoami")
	if err != nil {
		t.Fatalf("whoami: %v (%s)", err, out)
	}
	for _, want := range []string{"owner: local-owner", "terminal: owner", "identities: discord=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("whoami missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, testOwnerDiscordID) {
		t.Errorf("whoami printed an identity value:\n%s", out)
	}
}

func TestOwnerRekeyDryRunApplyAndAudit(t *testing.T) {
	cfgPath := writeOwnerCLIConfig(t)
	current, err := user.Current()
	if err != nil {
		t.Fatalf("os user unavailable: %v", err)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	sessions := store.NewSessionService(db)
	now := time.Now().UTC()
	for id, owner := range map[string]string{
		"terminal-1":                 current.Username,
		"cron:job-1":                 "cron",
		"dm:" + testOwnerDiscordID:   testOwnerDiscordID,
		"dm:" + testTrustedDiscordID: testTrustedDiscordID,
	} {
		if err := sessions.Create(t.Context(), &store.Session{ID: id, OwnerID: owner, CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
			t.Fatalf("create session %s: %v", id, err)
		}
	}
	_ = db.Close()

	out, err := runOwnerCommand(t, cfgPath, "rekey")
	if err != nil {
		t.Fatalf("rekey dry-run: %v (%s)", err, out)
	}
	if !strings.Contains(out, "dry-run") || !strings.Contains(out, "session: 4") {
		t.Fatalf("dry-run out = %q", out)
	}
	out, err = runOwnerCommand(t, cfgPath, "rekey", "--apply")
	if err != nil {
		t.Fatalf("rekey apply: %v (%s)", err, out)
	}
	if !strings.Contains(out, "applied") || !strings.Contains(out, "session: 4") {
		t.Fatalf("apply out = %q", out)
	}
	for _, leak := range []string{testOwnerDiscordID, testTrustedDiscordID, current.Username} {
		if leak != "" && strings.Contains(out, leak) {
			t.Errorf("rekey output leaks %q:\n%s", leak, out)
		}
	}

	db, err = openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	owners := map[string]string{}
	rows, err := db.QueryContext(t.Context(), `SELECT id, owner_id FROM session WHERE id NOT LIKE 'profile-%'`)
	if err != nil {
		t.Fatalf("query sessions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			t.Fatalf("scan: %v", err)
		}
		owners[id] = owner
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := map[string]string{
		"terminal-1":                 principal.OwnerID,
		"cron:job-1":                 principal.OwnerID,
		"dm:" + testOwnerDiscordID:   principal.OwnerID,
		"dm:" + testTrustedDiscordID: "discord:" + testTrustedDiscordID,
	}
	for id, owner := range want {
		if owners[id] != owner {
			t.Errorf("session %s owner = %q, want %q", id, owners[id], owner)
		}
	}
	var payload string
	if err := db.QueryRowContext(t.Context(), `SELECT payload_json FROM runtime_event WHERE kind = ?`, ownerRekeyEventKind).Scan(&payload); err != nil {
		t.Fatalf("rekey audit event: %v", err)
	}
	if !strings.Contains(payload, `"sessions":4`) || strings.Contains(payload, testOwnerDiscordID) {
		t.Errorf("audit payload = %s", payload)
	}

	out, err = runOwnerCommand(t, cfgPath, "rekey", "--apply")
	if err != nil || !strings.Contains(out, "session: 0") {
		t.Fatalf("second apply = %q, %v; want zero moves", out, err)
	}
}

func TestOwnerRekeyMoves(t *testing.T) {
	cfg := config.Default()
	cfg.Owner = &config.Owner{Identities: []string{"discord:" + testOwnerDiscordID}}
	cfg.Channels.Discord.AllowedUserIDs = []string{testTrustedDiscordID, testOwnerDiscordID}
	cases := map[string]struct {
		osUser string
		want   []store.OwnerMove
	}{
		"os user moves to owner": {osUser: "kuma", want: []store.OwnerMove{
			{From: "kuma", To: principal.OwnerID},
			{From: "cron", To: principal.OwnerID},
			{From: testOwnerDiscordID, To: principal.OwnerID},
			{From: testTrustedDiscordID, To: "discord:" + testTrustedDiscordID},
		}},
		"os user already canonical": {osUser: principal.OwnerID, want: []store.OwnerMove{
			{From: "cron", To: principal.OwnerID},
			{From: testOwnerDiscordID, To: principal.OwnerID},
			{From: testTrustedDiscordID, To: "discord:" + testTrustedDiscordID},
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			moves, err := ownerRekeyMoves(&cfg, tc.osUser)
			if err != nil {
				t.Fatalf("ownerRekeyMoves: %v", err)
			}
			if len(moves) != len(tc.want) {
				t.Fatalf("moves = %+v, want %+v", moves, tc.want)
			}
			for i := range moves {
				if moves[i] != tc.want[i] {
					t.Errorf("move[%d] = %+v, want %+v", i, moves[i], tc.want[i])
				}
			}
		})
	}
}

func readFileForTest(path string) (string, error) {
	raw, err := os.ReadFile(path)
	return string(raw), err
}

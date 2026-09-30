package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/profile"
	"github.com/anggasct/aura/internal/store"
)

func writeProfileCLIConfig(t *testing.T) string {
	t.Helper()
	dataRoot := t.TempDir()
	path := dataRoot + "/config.yaml"
	content := `version: 1
storage:
  path: ` + dataRoot + `
tools:
  workspace: ` + dataRoot + `
skills:
  roots: [` + dataRoot + `]
context:
  recent_complete_turns: 5
profile:
  prompt_version: v1
children:
  recovery: interrupt
`
	if err := writeFileForTest(path, content); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func runProfileCommand(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	gf := &globalFlags{configPath: cfg}
	cmd := newProfileCmd(gf)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestProfileCLIListEmpty(t *testing.T) {
	out, err := runProfileCommand(t, writeProfileCLIConfig(t), "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.TrimSpace(out) != "no facts" {
		t.Errorf("out = %q", out)
	}
}

func TestProfileCLISetShowAcceptRejectDelete(t *testing.T) {
	cfg := writeProfileCLIConfig(t)
	out, err := runProfileCommand(t, cfg, "set", "--category", "language", "--key", "backend", "--value", "Go")
	if err != nil {
		t.Fatalf("set: %v (%s)", err, out)
	}
	id := strings.TrimPrefix(strings.TrimSpace(out), "set: ")
	if id == "" || !strings.HasPrefix(id, "pf-") {
		t.Fatalf("set out = %q", out)
	}
	out, err = runProfileCommand(t, cfg, "show", id)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	for _, want := range []string{"category: language", "key: backend", "origin: owner", "verified"} {
		if !strings.Contains(out, want) {
			t.Errorf("show missing %q:\n%s", want, out)
		}
	}
	out, err = runProfileCommand(t, cfg, "list", "--status", "active")
	if err != nil || !strings.Contains(out, id) {
		t.Errorf("list active = %q, %v", out, err)
	}
	candidate := seedProfileCandidate(t, cfg)
	out, err = runProfileCommand(t, cfg, "review")
	if err != nil || !strings.Contains(out, candidate) {
		t.Errorf("review = %q, %v", out, err)
	}
	if pos := strings.Index(out, candidate); pos == -1 {
		t.Errorf("review missing candidate")
	}
	out, err = runProfileCommand(t, cfg, "accept", candidate)
	if err != nil || strings.TrimSpace(out) != "accepted: "+candidate {
		t.Errorf("accept = %q, %v", out, err)
	}
	rival := seedProfileConflictingCandidate(t, cfg)
	out, err = runProfileCommand(t, cfg, "review")
	if err != nil || !strings.Contains(out, "conflicts-with="+id) {
		t.Errorf("review conflict = %q, %v", out, err)
	}
	_ = rival
	out, err = runProfileCommand(t, cfg, "reject", rival)
	if err != nil || strings.TrimSpace(out) != "rejected: "+rival {
		t.Errorf("reject = %q, %v", out, err)
	}
	out, err = runProfileCommand(t, cfg, "delete", id)
	if err == nil {
		t.Errorf("delete guard = %q, want usage error", out)
	} else {
		var ue *usageError
		if !errors.As(err, &ue) {
			t.Errorf("delete guard err = %v (%q), want usage error", err, out)
		}
	}
	out, err = runProfileCommand(t, cfg, "delete", id, "--force")
	if err != nil || strings.TrimSpace(out) != "deleted: "+id {
		t.Errorf("delete = %q, %v", out, err)
	}
}

func seedProfileCandidate(t *testing.T, cfg string) string {
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
	sessions := store.NewSessionService(db)
	now := time.Now().UTC()
	if err := sessions.Create(t.Context(), &store.Session{ID: "sess-prof", OwnerID: profileLocalOwner, CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	events := store.NewEventStore(db)
	if _, err := events.AppendSequenced(t.Context(), "sess-prof", &store.RuntimeEvent{
		ID: "evt-prof-1", SessionID: "sess-prof", TurnID: "t1", InvocationID: "inv-1",
		Author: profileLocalOwner, Kind: "message.completed", SchemaVersion: 1,
		Payload: []byte(`{"text":"i use neovim"}`), CreatedAt: now,
	}); err != nil {
		t.Fatalf("AppendSequenced: %v", err)
	}
	service, err := profile.NewService(newProfileRegistry(db), profile.Config{MinConfidence: 0.99})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	fact, err := service.ProposeFact(t.Context(), profileLocalOwner, "tool", "editor", "neovim", &profile.Evidence{
		SourceEventID: "evt-prof-1", SourceDigest: profile.ValueDigest("i use neovim"),
		Provider: "p", Model: "m", PromptVersion: "v1", ObservedAt: now,
	}, now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	return fact.ID
}

func seedProfileConflictingCandidate(t *testing.T, cfg string) string {
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
	sessions := store.NewSessionService(db)
	now := time.Now().UTC()
	if err := sessions.Create(t.Context(), &store.Session{ID: "sess-conf", OwnerID: profileLocalOwner, CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	events := store.NewEventStore(db)
	if _, err := events.AppendSequenced(t.Context(), "sess-conf", &store.RuntimeEvent{
		ID: "evt-conf-1", SessionID: "sess-conf", TurnID: "t1", InvocationID: "inv-1",
		Author: profileLocalOwner, Kind: "message.completed", SchemaVersion: 1,
		Payload: []byte(`{"text":"switching to rust"}`), CreatedAt: now,
	}); err != nil {
		t.Fatalf("AppendSequenced: %v", err)
	}
	service, err := profile.NewService(newProfileRegistry(db), profile.Config{MinConfidence: 0.99})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	fact, err := service.ProposeFact(t.Context(), profileLocalOwner, "language", "backend", "Rust", &profile.Evidence{
		SourceEventID: "evt-conf-1", SourceDigest: profile.ValueDigest("switching to rust"),
		Provider: "p", Model: "m", PromptVersion: "v1", ObservedAt: now,
	}, now)
	if err != nil {
		t.Fatalf("ProposeFact: %v", err)
	}
	return fact.ID
}

func TestProfileCLISetUsageErrors(t *testing.T) {
	cfg := writeProfileCLIConfig(t)
	for name, args := range map[string][]string{
		"missing category": {"set", "--key", "k", "--value", "v"},
		"missing value":    {"set", "--category", "tool", "--key", "k"},
		"past expiry":      {"set", "--category", "tool", "--key", "k", "--value", "v", "--expires-at", "2020-01-01T00:00:00Z"},
		"bad expiry":       {"set", "--category", "tool", "--key", "k", "--value", "v", "--expires-at", "soon"},
		"bad category":     {"set", "--category", "mood", "--key", "k", "--value", "v"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runProfileCommand(t, cfg, args...)
			if err == nil {
				t.Errorf("expected usage error")
			}
		})
	}
}

func TestProfileCLIShowNotFound(t *testing.T) {
	_, err := runProfileCommand(t, writeProfileCLIConfig(t), "show", "pf-missing")
	if err == nil {
		t.Errorf("expected error")
	}
}

func TestProfileCLIRequiresEnabled(t *testing.T) {
	dataRoot := t.TempDir()
	path := dataRoot + "/config.yaml"
	content := `version: 1
storage:
  path: ` + dataRoot + `
tools:
  workspace: ` + dataRoot + `
skills:
  roots: [` + dataRoot + `]
context:
  recent_complete_turns: 5
`
	if err := writeFileForTest(path, content); err != nil {
		t.Fatal(err)
	}
	_, err := runProfileCommand(t, path, "list")
	if err == nil || !strings.Contains(err.Error(), "profiling") {
		t.Errorf("err = %v", err)
	}
}

func TestProfileActionSinkRecordsEvents(t *testing.T) {
	cfg := writeProfileCLIConfig(t)
	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	sink := newProfileActionSink(db, profileLocalOwner)
	action := &profile.OwnerAction{OwnerID: profileLocalOwner, FactID: "pf-1", Action: "accept", Category: "tool", Key: "editor", At: time.Now().UTC()}
	if err := sink.RecordOwnerAction(t.Context(), action); err != nil {
		t.Fatalf("RecordOwnerAction: %v", err)
	}
	if err := sink.RecordOwnerAction(t.Context(), action); err != nil {
		t.Fatalf("idempotent RecordOwnerAction: %v", err)
	}
	if err := sink.RecordOwnerAction(t.Context(), nil); err == nil {
		t.Errorf("expected nil rejection")
	}
	var count int
	if err := db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM runtime_event WHERE kind = 'profile.owner_action.v1'`,
	).Scan(&count); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if count != 1 {
		t.Errorf("events = %d, want 1 (idempotent)", count)
	}
}

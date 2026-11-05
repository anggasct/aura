package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/store"
)

func seedAPIKey(t *testing.T, keys store.APIKeyStore, id, class string, at time.Time) {
	t.Helper()
	key := &store.APIKey{ID: id, KeySHA256: strings.Repeat("a", 52) + strings.ToLower(id), PersonaID: "main", Class: class, Label: "client", CreatedAt: at}
	if err := keys.Create(t.Context(), key); err != nil {
		t.Fatalf("Create key %s: %v", id, err)
	}
}

func TestPrincipalAdminCoversHTTPKeys(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	db := openAccessDB(t, cfg)
	now := time.Now().UTC()
	keys := store.NewAPIKeyStore(db)
	seedAPIKey(t, keys, "aaaaaaaaaaaa", store.APIKeyClassPublic, now)
	seedAPIKey(t, keys, "bbbbbbbbbbbb", store.APIKeyClassTrusted, now)
	seedAPIKey(t, keys, "cccccccccccc", store.APIKeyClassPublic, now)
	seedAPIKey(t, keys, "dddddddddddd", store.APIKeyClassOwner, now)
	if _, err := keys.Revoke(t.Context(), "cccccccccccc", now); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	seedIdleSession(t, store.NewSessionService(db), "main/http/aaaaaaaaaaaa/one", "http:aaaaaaaaaaaa", now)

	out, err := runAccessCommand(t, newPrincipalCmd, cfg, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"http:aaaaaaaaaaaa\tpublic\tkey:main\t1\t", "http:bbbbbbbbbbbb\ttrusted\tkey:main\t0\t-", "http:cccccccccccc\tpublic\trevoked-key:main\t0\t-"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output %q missing %q", out, want)
		}
	}
	if strings.Contains(out, "dddddddddddd") {
		t.Errorf("list output shows an owner key: %q", out)
	}
	if out, err := runAccessCommand(t, newPrincipalCmd, cfg, "show", "http:bbbbbbbbbbbb"); err != nil || !strings.Contains(out, "class: trusted") {
		t.Errorf("show = %q, %v", out, err)
	}
	if out, err := runAccessCommand(t, newPrincipalCmd, cfg, "forget", "http:aaaaaaaaaaaa", "--apply"); err != nil || !strings.Contains(out, "sessions: 1") {
		t.Fatalf("forget = %q, %v", out, err)
	}
	if sessionExists(t, db, "main/http/aaaaaaaaaaaa/one") {
		t.Error("forget left the key's session")
	}
	if key, err := keys.GetByID(t.Context(), "aaaaaaaaaaaa"); err != nil || !key.Active() {
		t.Errorf("forget changed the key: %+v, %v", key, err)
	}
}

func TestRetentionSweepErasesIdleHTTPPrincipalsByKeyClass(t *testing.T) {
	cfg := writePrincipalCLIConfig(t)
	db := openAccessDB(t, cfg)
	loaded, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	keys := store.NewAPIKeyStore(db)
	seedAPIKey(t, keys, "aaaaaaaaaaaa", store.APIKeyClassPublic, now.Add(-9000*time.Hour))
	seedAPIKey(t, keys, "bbbbbbbbbbbb", store.APIKeyClassTrusted, now.Add(-9000*time.Hour))
	sessions := store.NewSessionService(db)
	seedIdleSession(t, sessions, "main/http/aaaaaaaaaaaa/one", "http:aaaaaaaaaaaa", now.Add(-721*time.Hour))
	seedIdleSession(t, sessions, "main/http/bbbbbbbbbbbb/one", "http:bbbbbbbbbbbb", now.Add(-721*time.Hour))
	seedIdleSession(t, sessions, "main/http/dddddddddddd/one", principal.OwnerID, now.Add(-9000*time.Hour))

	swept, err := sweepRetention(t.Context(), loaded.Config, db, "", now)
	if err != nil {
		t.Fatalf("sweepRetention: %v", err)
	}
	if swept[principal.ClassPublic] != 1 || swept[principal.ClassTrusted] != 0 {
		t.Fatalf("swept = %v, want one public", swept)
	}
	for id, want := range map[string]bool{
		"main/http/aaaaaaaaaaaa/one": false,
		"main/http/bbbbbbbbbbbb/one": true,
		"main/http/dddddddddddd/one": true,
	} {
		if exists := sessionExists(t, db, id); exists != want {
			t.Errorf("session %s exists = %v, want %v", id, exists, want)
		}
	}
	if key, err := keys.GetByID(t.Context(), "aaaaaaaaaaaa"); err != nil || !key.Active() {
		t.Errorf("retention changed the key: %+v, %v", key, err)
	}
}

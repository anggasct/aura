package mcp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFileTokenStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	store, err := NewFileTokenStore(path)
	if err != nil {
		t.Fatalf("NewFileTokenStore(): %v", err)
	}
	if _, found, err := store.Load(); err != nil || found {
		t.Fatalf("Load() = %v, %v, want missing", found, err)
	}
	record := TokenRecord{
		AccessToken: "access-1", RefreshToken: "refresh-1",
		TokenType: "Bearer", Expiry: time.Now().Add(time.Hour).UTC().Truncate(time.Second),
	}
	if err := store.Store(record); err != nil {
		t.Fatalf("Store(): %v", err)
	}
	loaded, found, err := store.Load()
	if err != nil || !found {
		t.Fatalf("Load() = %+v, %v, %v", loaded, found, err)
	}
	if loaded != record {
		t.Errorf("loaded = %+v, want %+v", loaded, record)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(): %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestFileTokenStoreCorruptFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store, err := NewFileTokenStore(path)
	if err != nil {
		t.Fatalf("NewFileTokenStore(): %v", err)
	}
	if _, _, err := store.Load(); err == nil {
		t.Fatal("corrupt store accepted")
	} else if _, ok := CodeOf(err); !ok {
		t.Errorf("uncoded error: %v", err)
	}
}

func TestFileTrustRegistryPersistsApprovals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	first, err := NewFileTrustRegistry(path)
	if err != nil {
		t.Fatalf("NewFileTrustRegistry(): %v", err)
	}
	ctx := t.Context()
	if err := first.ApproveConfig(ctx, "srv", "config-1"); err != nil {
		t.Fatalf("ApproveConfig(): %v", err)
	}
	if err := first.SaveSessionTrust(ctx, "srv", "digest-1", []string{"tools"}, []string{"echo"}); err != nil {
		t.Fatalf("SaveSessionTrust(): %v", err)
	}
	if trusted, err := first.IsTrusted(ctx, "srv", "digest-1"); err != nil || trusted {
		t.Fatalf("unapproved trust accepted: %v, %v", trusted, err)
	}
	if err := first.Approve(ctx, "srv", "digest-1"); err != nil {
		t.Fatalf("Approve(): %v", err)
	}

	second, err := NewFileTrustRegistry(path)
	if err != nil {
		t.Fatalf("NewFileTrustRegistry(): %v", err)
	}
	if trusted, err := second.IsTrusted(ctx, "srv", "digest-1"); err != nil || !trusted {
		t.Errorf("approval lost across restart: %v, %v", trusted, err)
	}
	if trusted, err := second.IsConfigTrusted(ctx, "srv", "config-1"); err != nil || !trusted {
		t.Errorf("config approval lost across restart: %v, %v", trusted, err)
	}
	record, err := second.GetTrust(ctx, "srv")
	if err != nil {
		t.Fatalf("GetTrust(): %v", err)
	}
	if len(record.Tools) != 1 || record.Tools[0] != "echo" {
		t.Errorf("record = %+v", record)
	}
	if err := second.ApproveConfig(ctx, "srv", "config-1"); err != nil {
		t.Fatalf("ApproveConfig(same): %v", err)
	}
	if err := second.SaveSessionTrust(ctx, "srv", "digest-2", nil, nil); err != nil {
		t.Fatalf("SaveSessionTrust(): %v", err)
	}
	if trusted, err := second.IsTrusted(ctx, "srv", "digest-1"); err != nil || trusted {
		t.Errorf("stale digest still trusted after change: %v, %v", trusted, err)
	}
}

func TestFileTrustRegistryCorruptFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	if err := os.WriteFile(path, []byte("[broken"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	registry, err := NewFileTrustRegistry(path)
	if err != nil {
		t.Fatalf("NewFileTrustRegistry(): %v", err)
	}
	if _, err := registry.IsTrusted(t.Context(), "srv", "digest"); err == nil {
		t.Error("corrupt registry accepted")
	}
}

func TestFileTrustRegistryWritesOwnerOnlyAtomically(t *testing.T) {
	ctx := t.Context()
	dir := filepath.Join(t.TempDir(), "mcp")
	path := filepath.Join(dir, "trust.json")
	registry, err := NewFileTrustRegistry(path)
	if err != nil {
		t.Fatalf("NewFileTrustRegistry(): %v", err)
	}
	for i := range 3 {
		if err := registry.ApproveConfig(ctx, "srv", "config-"+string(rune('a'+i))); err != nil {
			t.Fatalf("ApproveConfig(): %v", err)
		}
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(file): %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 600", fileInfo.Mode().Perm())
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat(dir): %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 700", dirInfo.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(): %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the registry file", len(entries))
	}
}

func TestFileTrustRegistryCorruptFileIsNeverOverwritten(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "trust.json")
	corrupt := []byte("[broken")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	registry, err := NewFileTrustRegistry(path)
	if err != nil {
		t.Fatalf("NewFileTrustRegistry(): %v", err)
	}
	if _, err := registry.IsConfigTrusted(ctx, "srv", "cfg"); err == nil {
		t.Fatal("corrupt registry read accepted")
	}
	if err := registry.ApproveConfig(ctx, "srv", "cfg"); err == nil {
		t.Fatal("approval written over a corrupt registry")
	}
	if _, err := registry.Revoke(ctx, "srv"); err == nil {
		t.Fatal("revoke succeeded over a corrupt registry")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(): %v", err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Error("corrupt registry was rewritten")
	}
}

func TestFileTrustRegistryRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trust.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"servers":{}}`), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	registry, err := NewFileTrustRegistry(path)
	if err != nil {
		t.Fatalf("NewFileTrustRegistry(): %v", err)
	}
	if _, err := registry.IsConfigTrusted(t.Context(), "srv", "cfg"); err == nil {
		t.Fatal("unknown registry version accepted")
	}
}

func TestFileTrustRegistryAcrossInstancesSeesLatestWrites(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "trust.json")
	daemon, err := NewFileTrustRegistry(path)
	if err != nil {
		t.Fatalf("NewFileTrustRegistry(): %v", err)
	}
	cli, err := NewFileTrustRegistry(path)
	if err != nil {
		t.Fatalf("NewFileTrustRegistry(): %v", err)
	}
	if trusted, err := daemon.IsConfigTrusted(ctx, "srv", "cfg"); err != nil || trusted {
		t.Fatalf("empty registry trusted: %v, %v", trusted, err)
	}
	if err := cli.ApproveConfig(ctx, "srv", "cfg"); err != nil {
		t.Fatalf("ApproveConfig(): %v", err)
	}
	if trusted, err := daemon.IsConfigTrusted(ctx, "srv", "cfg"); err != nil || !trusted {
		t.Fatalf("approval written by another process not seen: %v, %v", trusted, err)
	}
	if err := daemon.SaveSessionTrust(ctx, "srv", "tools", nil, nil); err != nil {
		t.Fatalf("SaveSessionTrust(): %v", err)
	}
	if trusted, err := cli.IsConfigTrusted(ctx, "srv", "cfg"); err != nil || !trusted {
		t.Fatalf("daemon write dropped the config approval: %v, %v", trusted, err)
	}
}

package mcp

import (
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
	if err := first.SaveSessionTrust(ctx, "srv", "digest-1", []string{"tools"}, []string{"echo"}); err != nil {
		t.Fatalf("SaveSessionTrust(): %v", err)
	}
	if trusted, err := first.IsTrusted(ctx, "srv", "digest-1"); err != nil || trusted {
		t.Fatalf("unapproved trust accepted: %v, %v", trusted, err)
	}
	if err := first.Approve(ctx, "srv", "digest-1"); err != nil {
		t.Fatalf("Approve(): %v", err)
	}
	if err := first.SaveSpawnTrust(ctx, "srv", "spawn-1"); err != nil {
		t.Fatalf("SaveSpawnTrust(): %v", err)
	}
	if err := first.ApproveSpawn(ctx, "srv", "spawn-1"); err != nil {
		t.Fatalf("ApproveSpawn(): %v", err)
	}

	second, err := NewFileTrustRegistry(path)
	if err != nil {
		t.Fatalf("NewFileTrustRegistry(): %v", err)
	}
	if trusted, err := second.IsTrusted(ctx, "srv", "digest-1"); err != nil || !trusted {
		t.Errorf("approval lost across restart: %v, %v", trusted, err)
	}
	if trusted, err := second.IsSpawnTrusted(ctx, "srv", "spawn-1"); err != nil || !trusted {
		t.Errorf("spawn approval lost across restart: %v, %v", trusted, err)
	}
	record, err := second.GetTrust(ctx, "srv")
	if err != nil {
		t.Fatalf("GetTrust(): %v", err)
	}
	if len(record.Tools) != 1 || record.Tools[0] != "echo" {
		t.Errorf("record = %+v", record)
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

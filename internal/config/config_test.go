package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testdataDir = "../../testdata"

func fixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(testdataDir, "config", name)
}

func TestMarshalDefault_GoldenFile(t *testing.T) {
	got, err := Marshal(Default())
	if err != nil {
		t.Fatalf("Marshal(Default()): %v", err)
	}
	want, err := os.ReadFile(filepath.Join(testdataDir, "golden", "config.yaml"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("golden mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestLoad_ValidFile(t *testing.T) {
	cfg, err := Load(fixture(t, "valid.yaml"))
	if err != nil {
		t.Fatalf("Load(valid): %v", err)
	}
	if cfg.Version != 1 {
		t.Errorf("Version = %d, want 1", cfg.Version)
	}
	if cfg.Server.Host != "0.0.0.0" || cfg.Server.Port != 9090 {
		t.Errorf("Server = %+v, want host 0.0.0.0 port 9090", cfg.Server)
	}
	if got := time.Duration(cfg.Server.ShutdownTimeout); got != 10*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 10s", got)
	}
	if cfg.Logging.Level != "debug" || cfg.Logging.Format != "json" {
		t.Errorf("Logging = %+v, want debug/json", cfg.Logging)
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	t.Setenv("AURA_SERVER_PORT", "7777")
	t.Setenv("AURA_SERVER_SHUTDOWN_TIMEOUT", "45s")
	t.Setenv("AURA_LOGGING_LEVEL", "warn")

	cfg, err := Load(fixture(t, "valid.yaml"))
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg.Server.Port != 7777 {
		t.Errorf("Port = %d, want 7777 (env override)", cfg.Server.Port)
	}
	if got := time.Duration(cfg.Server.ShutdownTimeout); got != 45*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 45s (env override)", got)
	}
	if cfg.Logging.Level != "warn" {
		t.Errorf("Level = %q, want warn (env override)", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("Format = %q, want json (unchanged from file)", cfg.Logging.Format)
	}
}

func TestLoad_EnvOverrideDoesNotPersist(t *testing.T) {
	src := fixture(t, "valid.yaml")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	t.Setenv("AURA_SERVER_PORT", "5555")
	if _, err := Load(dst); err != nil {
		t.Fatalf("Load: %v", err)
	}
	after, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(after) != string(data) {
		t.Fatalf("config file was modified by env override\nbefore:\n%s\nafter:\n%s", data, after)
	}
}

func TestLoad_MissingDefaultAutoGenerates(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	if cfg.Server.Port != 8280 {
		t.Errorf("Port = %d, want default 8280", cfg.Server.Port)
	}

	path := filepath.Join(xdg, "aura", "config.yaml")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected generated config at %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("generated config perm = %o, want 600", perm)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated: %v", err)
	}
	want, err := Marshal(Default())
	if err != nil {
		t.Fatalf("marshal default: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("generated config != Default()\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestLoad_MalformedYAML(t *testing.T) {
	_, err := Load(fixture(t, "invalid.yaml"))
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
	if !strings.Contains(err.Error(), "invalid YAML") {
		t.Errorf("error %q does not mention invalid YAML", err)
	}
	if !strings.Contains(err.Error(), "line") {
		t.Errorf("error %q does not specify a line", err)
	}
}

func TestLoad_UnknownKey(t *testing.T) {
	_, err := Load(fixture(t, "unknown_keys.yaml"))
	if err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
	if !strings.Contains(err.Error(), `unknown key "server.typo"`) {
		t.Errorf("error %q does not name the offending key", err)
	}
	if !strings.Contains(err.Error(), "line") {
		t.Errorf("error %q does not specify a line", err)
	}
}

func TestLoad_WrongVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 2\nserver:\n  host: 127.0.0.1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unsupported version, got nil")
	}
	if !strings.Contains(err.Error(), `unsupported version "2"`) {
		t.Errorf("error %q does not report unsupported version", err)
	}
}

func TestLoad_ExplicitPathMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error for missing explicit path, got nil")
	}
	if !strings.Contains(err.Error(), "file not found") {
		t.Errorf("error %q does not report file not found", err)
	}
}

func TestLoad_PathIsDirectory(t *testing.T) {
	_, err := Load(t.TempDir())
	if err == nil {
		t.Fatal("expected error when config path is a directory, got nil")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("error %q does not report directory", err)
	}
}

func TestResolvePath_XDGEmptyFallsBack(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	path, explicit, err := resolvePath("")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if explicit {
		t.Error("explicit = true, want false for default resolution")
	}
	want := filepath.Join(home, ".config", "aura", "config.yaml")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

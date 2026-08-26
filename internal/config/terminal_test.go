package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeConfigYAML(t *testing.T, extra string) LoadResult {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	full := "version: 1\n" + extra
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	result, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return result
}

func TestLoadTerminalDefaultsApplied(t *testing.T) {
	result := writeConfigYAML(t, "")
	term := result.Config.Terminal
	if term.RenderHz != 20 {
		t.Errorf("RenderHz = %d, want 20", term.RenderHz)
	}
	if term.MaxInputBytes != 262144 {
		t.Errorf("MaxInputBytes = %d, want 262144", term.MaxInputBytes)
	}
	if term.InMemoryHistory != 100 {
		t.Errorf("InMemoryHistory = %d, want 100", term.InMemoryHistory)
	}
	if time.Duration(term.SecondInterruptTime) != 2*time.Second {
		t.Errorf("SecondInterruptTime = %v, want 2s", term.SecondInterruptTime)
	}
	if term.PlainApproval != "deny" {
		t.Errorf("PlainApproval = %q, want deny", term.PlainApproval)
	}
}

func TestLoadTerminalOverrides(t *testing.T) {
	result := writeConfigYAML(t, "terminal:\n  render_hz: 30\n  max_input_bytes: 4096\n  in_memory_history: 256\n  second_interrupt_window: 3s\n  plain_approval: \"deny\"\n")
	term := result.Config.Terminal
	if term.RenderHz != 30 || term.MaxInputBytes != 4096 || term.InMemoryHistory != 256 {
		t.Errorf("terminal overrides not applied: %+v", term)
	}
	if time.Duration(term.SecondInterruptTime) != 3*time.Second {
		t.Errorf("SecondInterruptTime = %v, want 3s", term.SecondInterruptTime)
	}
}

func TestValidateTerminalRejectsInvalidApproval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nterminal:\n  plain_approval: \"yes\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid plain_approval to fail")
	}
}

func TestValidateTerminalRejectsNegativeHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nterminal:\n  in_memory_history: -1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected negative in_memory_history to fail")
	}
}

func TestTerminalShapeRejectsWrongTypes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nterminal:\n  render_hz: \"fast\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected type error for render_hz string")
	}
}

func TestTerminalUnknownKeyRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nterminal:\n  bogus_key: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown terminal key to fail")
	}
}

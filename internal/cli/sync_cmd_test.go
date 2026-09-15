package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/sync"
)

func writeSyncCLIConfig(t *testing.T, syncSection string) string {
	t.Helper()
	dataRoot := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
storage:
  path: ` + dataRoot + `
` + syncSection
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func disabledSyncConfig(t *testing.T) string {
	t.Helper()
	return writeSyncCLIConfig(t, `sync:
  enabled: false
  remote: "ssh://git@example.com/owner/aura-assets.git"
  branch: "main"
  interval: 15m
  transport_secret_ref: "env://AURA_SYNC_TRANSPORT_SECRET"
  known_hosts_ref: "env://AURA_SYNC_KNOWN_HOSTS"
  git_binary: "/usr/bin/git"
  ssh_binary: "/usr/bin/ssh"
  include:
    - "skills/**"
`)
}

func runSyncCommand(t *testing.T, cfg string, args ...string) (string, error) {
	t.Helper()
	gf := &globalFlags{configPath: cfg}
	cmd := newSyncCmd(gf)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestSyncStatusDisabled(t *testing.T) {
	t.Parallel()
	out, err := runSyncCommand(t, disabledSyncConfig(t), "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{"state: disabled", "local_ref: -", "remote_ref: -", "last_verified_at: -"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output %q misses %q", out, want)
		}
	}
}

func TestSyncDoctorDisabled(t *testing.T) {
	t.Parallel()
	out, err := runSyncCommand(t, disabledSyncConfig(t), "doctor")
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	for _, want := range []string{"gate: disabled", "profile: ok", "transport: ok", "manifest: ok", "refs: ok"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output %q misses %q", out, want)
		}
	}
}

func TestSyncRunRefusedWhenDisabled(t *testing.T) {
	t.Parallel()
	_, err := runSyncCommand(t, disabledSyncConfig(t), "run")
	if err == nil {
		t.Fatal("run without gate must fail")
	}
	if code, ok := sync.CodeOf(err); !ok || code != sync.ErrorCodeGateRefused {
		t.Fatalf("run code = %v,%v want sync_unavailable", code, ok)
	}
}

func TestSyncReconcileWithoutUnknown(t *testing.T) {
	t.Parallel()
	_, err := runSyncCommand(t, disabledSyncConfig(t), "reconcile")
	if err == nil {
		t.Fatal("reconcile without gate must fail")
	}
	if runtime.GOOS == "linux" {
		enabled := writeSyncCLIConfig(t, `sync:
  enabled: true
  remote: "ssh://git@example.com/owner/aura-assets.git"
  branch: "main"
  interval: 15m
  transport_secret_ref: "env://AURA_SYNC_TRANSPORT_SECRET"
  known_hosts_ref: "env://AURA_SYNC_KNOWN_HOSTS"
  git_binary: "/usr/bin/git"
  ssh_binary: "/usr/bin/ssh"
  include:
    - "skills/**"
`)
		_, err = runSyncCommand(t, enabled, "reconcile")
		if err == nil {
			t.Fatal("reconcile without unknown must fail")
		}
		if code, ok := sync.CodeOf(err); !ok || code != sync.ErrorCodeNoUnknownState {
			t.Fatalf("reconcile code = %v,%v want sync_no_unknown_state", code, ok)
		}
	} else {
		t.Logf("linux-only gate assertion skipped on %s", runtime.GOOS)
	}
}

func TestSyncDoctorRejectsManifest(t *testing.T) {
	t.Parallel()
	cfg := writeSyncCLIConfig(t, `sync:
  enabled: false
  remote: "ssh://git@example.com/owner/aura-assets.git"
  branch: "main"
  interval: 15m
  transport_secret_ref: "env://AURA_SYNC_TRANSPORT_SECRET"
  known_hosts_ref: "env://AURA_SYNC_KNOWN_HOSTS"
  git_binary: "/usr/bin/git"
  ssh_binary: "/usr/bin/ssh"
  include:
    - "workspace/**"
`)
	_, err := runSyncCommand(t, cfg, "doctor")
	if err == nil || !strings.Contains(err.Error(), "config_invalid") {
		t.Fatalf("doctor err = %v, want config_invalid", err)
	}
}

func writeSyncExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakebin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return path
}

func enabledSyncConfigWithBinaries(t *testing.T, gitBin, sshBin, include string) (cfgPath, dataRoot string) {
	t.Helper()
	dataRoot = t.TempDir()
	cfgPath = filepath.Join(t.TempDir(), "config.yaml")
	content := `version: 1
storage:
  path: ` + dataRoot + `
sync:
  enabled: true
  remote: "ssh://git@example.com/owner/aura-assets.git"
  branch: "main"
  interval: 15m
  transport_secret_ref: "env://AURA_SYNC_TRANSPORT_SECRET"
  known_hosts_ref: "env://AURA_SYNC_KNOWN_HOSTS"
  git_binary: "` + gitBin + `"
  ssh_binary: "` + sshBin + `"
  include:
` + include
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath, dataRoot
}

func TestSyncRunEnabledPrintsPassResult(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sync gate requires Linux")
	}
	gitBin := writeSyncExecutable(t)
	sshBin := writeSyncExecutable(t)
	cfg, _ := enabledSyncConfigWithBinaries(t, gitBin, sshBin, "    - \"skills/**\"\n")
	previous := syncRunPassFunc
	syncRunPassFunc = func(_ context.Context, _ *config.Config, _ *sync.StateStore) (sync.PassOutcome, error) {
		return sync.PassOutcome{
			State:     sync.WorkerIdle,
			LocalRef:  "local-abc123",
			RemoteRef: "remote-def456",
			Result:    "pushed",
		}, nil
	}
	t.Cleanup(func() { syncRunPassFunc = previous })
	out, err := runSyncCommand(t, cfg, "run")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"result: pushed", "local_ref: local-abc123", "remote_ref: remote-def456"} {
		if !strings.Contains(out, want) {
			t.Fatalf("run output %q misses %q", out, want)
		}
	}
	if strings.Contains(out, "result: unknown") {
		t.Fatalf("run output %q still reports hardcoded unknown", out)
	}
}

func TestSyncStatusSeesDurableCheckpoint(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sync gate requires Linux")
	}
	gitBin := writeSyncExecutable(t)
	sshBin := writeSyncExecutable(t)
	cfg, dataRoot := enabledSyncConfigWithBinaries(t, gitBin, sshBin, "    - \"skills/**\"\n")
	store, err := sync.NewFileStateStore(sync.SyncStatePath(dataRoot))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store.MarkConflict("local-conflict", "remote-conflict", time.Now().UTC())
	out, err := runSyncCommand(t, cfg, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{"state: conflict", "local_ref: local-conflict", "remote_ref: remote-conflict"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output %q misses %q", out, want)
		}
	}
}

func TestSyncReconcileFindsDurableUnknown(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("sync gate requires Linux")
	}
	gitBin := writeSyncExecutable(t)
	sshBin := writeSyncExecutable(t)
	cfg, dataRoot := enabledSyncConfigWithBinaries(t, gitBin, sshBin, "    - \"skills/**\"\n")
	store, err := sync.NewFileStateStore(sync.SyncStatePath(dataRoot))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	store.MarkUnknown("intent-durable-1", time.Now().UTC())
	out, err := runSyncCommand(t, cfg, "reconcile")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !strings.Contains(out, "result: unknown") {
		t.Fatalf("reconcile output %q misses unknown result", out)
	}
}

func TestSyncDoctorProfileInvalidOffline(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-git")
	sshBin := writeSyncExecutable(t)
	cfg, _ := enabledSyncConfigWithBinaries(t, missing, sshBin, "    - \"skills/**\"\n")
	out, err := runSyncCommand(t, cfg, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !strings.Contains(out, "profile: invalid: git_binary is not an executable file") {
		t.Fatalf("doctor output %q misses specific profile reason", out)
	}
	if !strings.Contains(out, "transport: ok") {
		t.Fatalf("doctor output %q should report transport ok offline, got %q", out, out)
	}
}

func TestSyncDoctorManifestInvalidOffline(t *testing.T) {
	gitBin := writeSyncExecutable(t)
	sshBin := writeSyncExecutable(t)
	cfg, _ := enabledSyncConfigWithBinaries(t, gitBin, sshBin, "    - \"skills/**\"\n    - \"skills/**\"\n")
	out, err := runSyncCommand(t, cfg, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !strings.Contains(out, "manifest: invalid:") || !strings.Contains(out, "listed more than once") {
		t.Fatalf("doctor output %q misses specific manifest reason", out)
	}
}

func TestSyncMissingConfigTyped(t *testing.T) {
	t.Parallel()
	dataRoot := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 1\nstorage:\n  path: " + dataRoot + "\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := runSyncCommand(t, cfgPath, "status")
	if err == nil {
		t.Fatal("status without sync section must fail")
	}
	if code, ok := sync.CodeOf(err); !ok || code != sync.ErrorCodeNotConfigured {
		t.Fatalf("code = %v,%v want sync_not_configured", code, ok)
	}
}

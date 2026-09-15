package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if err == nil || !strings.Contains(err.Error(), "sync_unavailable") {
		t.Fatalf("run err = %v, want sync_unavailable", err)
	}
}

func TestSyncReconcileWithoutUnknown(t *testing.T) {
	t.Parallel()
	_, err := runSyncCommand(t, disabledSyncConfig(t), "reconcile")
	if err == nil {
		t.Fatal("reconcile without gate must fail")
	}
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
	if err == nil || !strings.Contains(err.Error(), "sync_no_unknown_state") {
		t.Fatalf("reconcile err = %v, want sync_no_unknown_state", err)
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

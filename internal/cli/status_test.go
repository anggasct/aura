package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/store"
)

func TestFormatSandboxStatusAvailable(t *testing.T) {
	all := sandbox.Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}
	text, available := formatSandboxStatus(all, nil)
	if !available || text != "sandbox: available" {
		t.Fatalf("got %q available=%v", text, available)
	}
}

// The surface must name every absent primitive so an operator knows exactly
// what the host lacks, matching the Require gate's vocabulary.
func TestFormatSandboxStatusNamesMissing(t *testing.T) {
	partial := sandbox.Primitives{ProcessGroups: true}
	text, available := formatSandboxStatus(partial, nil)
	if available {
		t.Fatal("want unavailable")
	}
	for _, want := range []string{"cgroup_v2", "landlock", "seccomp", "user_namespace"} {
		if !strings.Contains(text, want) {
			t.Errorf("status %q missing primitive %q", text, want)
		}
	}
	if strings.Contains(text, "process_groups") {
		t.Errorf("status should not list a present primitive:\n%s", text)
	}
}

func TestFormatSandboxStatusReasonOnProbeError(t *testing.T) {
	text, available := formatSandboxStatus(sandbox.Primitives{}, errors.New("sandbox requires Linux"))
	if available || !strings.Contains(text, "sandbox requires Linux") {
		t.Fatalf("got %q available=%v", text, available)
	}
}

func TestStatusCmdReportsMissingPrimitive(t *testing.T) {
	negotiate := func() (sandbox.Primitives, error) {
		return sandbox.Primitives{ProcessGroups: true}, nil
	}
	cmd := newStatusCmdForTest(t, negotiate, false)
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err == nil {
		t.Fatal("want error when sandbox unavailable")
	}
	if !strings.Contains(out.String(), "user_namespace") {
		t.Errorf("output missing primitive name:\n%s", out.String())
	}
}

func TestStatusCmdAvailableExitsClean(t *testing.T) {
	negotiate := func() (sandbox.Primitives, error) {
		return sandbox.Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}, nil
	}
	cmd := newStatusCmdForTest(t, negotiate, true)
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !strings.Contains(out.String(), "sandbox: available") {
		t.Errorf("output=%q", out.String())
	}
}

// newStatusCmdForTest builds the status command against a fully seeded
// temporary environment: a migrated database, a fresh backup directory, and
// a configured primary model, so the healthy-path assertion is meaningful.
// healthyStorage=false removes the database and backup so degraded findings
// appear.
func newStatusCmdForTest(t *testing.T, negotiate func() (sandbox.Primitives, error), healthyStorage bool) *cobra.Command {
	t.Helper()
	dataRoot := t.TempDir()
	if healthyStorage {
		seedHealthyStorage(t, dataRoot)
	}
	cfg := config.Default()
	cfg.Storage.Path = dataRoot
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"primary": {Protocol: "anthropic", Model: "claude-sonnet-4"},
	}
	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(t.Context())
	cmd.RunE = func(c *cobra.Command, _ []string) error {
		return runStatus(c, &cfg, negotiate, true, false)
	}
	return cmd
}

func seedHealthyStorage(t *testing.T, dataRoot string) {
	t.Helper()
	db, err := store.OpenDB(t.Context(), filepath.Join(dataRoot, "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	backups := filepath.Join(dataRoot, "backups")
	if err := os.MkdirAll(filepath.Join(backups, "latest"), 0o700); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
}

// Offline output must label the live surface unreachable, list findings in a
// stable order, and map severity to the documented exit codes.
func TestStatusOfflineDeterministicOutputAndExitCodes(t *testing.T) {
	negotiate := func() (sandbox.Primitives, error) {
		return sandbox.Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}, nil
	}
	cmd := newStatusCmdForTest(t, negotiate, false)
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := cmd.RunE(cmd, nil)
	var coded *exitCodeError
	if !errors.As(err, &coded) || coded.code != exitCritical {
		t.Fatalf("RunE err = %v, want critical exit (no db, no backup)", err)
	}
	text := out.String()
	if !strings.Contains(text, "live: unreachable") {
		t.Errorf("offline output must label live unreachable:\n%s", text)
	}
	if !strings.Contains(text, "critical backup backup_missing") {
		t.Errorf("backup finding missing:\n%s", text)
	}
}

func TestStatusJSONEncodesContract(t *testing.T) {
	negotiate := func() (sandbox.Primitives, error) {
		return sandbox.Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}, nil
	}
	dataRoot := t.TempDir()
	seedHealthyStorage(t, dataRoot)
	cfg := config.Default()
	cfg.Storage.Path = dataRoot
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"primary": {Protocol: "anthropic", Model: "claude-sonnet-4"},
	}
	cmd := &cobra.Command{Use: "status"}
	cmd.SetContext(t.Context())
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := runStatus(cmd, &cfg, negotiate, true, true); err != nil {
		t.Fatalf("runStatus json: %v", err)
	}
	var decoded struct {
		Status   string `json:"status"`
		Severity string `json:"severity"`
		Live     struct {
			Reachable bool `json:"reachable"`
		} `json:"live"`
		Findings []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("decode json output: %v\n%s", err, out.String())
	}
	if decoded.Status != "up" || decoded.Severity != "info" {
		t.Errorf("status = %s/%s, want up/info", decoded.Status, decoded.Severity)
	}
	if decoded.Live.Reachable {
		t.Error("offline json must report live unreachable")
	}
	if len(decoded.Findings) != 5 {
		t.Fatalf("findings = %d, want 5 (migration, backup, storage, sandbox, provider): %v", len(decoded.Findings), decoded.Findings)
	}
	for _, f := range decoded.Findings {
		if f.ID == "" || f.Severity == "" {
			t.Errorf("finding lacks id/severity: %+v", f)
		}
	}
}

func TestDoctorFiltersByCheck(t *testing.T) {
	dataRoot := t.TempDir()
	seedHealthyStorage(t, dataRoot)
	cfg := config.Default()
	cfg.Storage.Path = dataRoot
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"primary": {Protocol: "anthropic", Model: "claude-sonnet-4"},
	}
	negotiate := func() (sandbox.Primitives, error) {
		return sandbox.Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}, nil
	}
	registry, err := buildHealthRegistry(&cfg, negotiate)
	if err != nil {
		t.Fatalf("buildHealthRegistry: %v", err)
	}
	findings := registry.Evaluate(t.Context())
	filtered := filterFindingsByCheck(findings, "backup")
	if len(filtered) != 1 {
		t.Fatalf("filtered = %d, want 1: %+v", len(filtered), filtered)
	}
	if filtered[0].Component != "backup" || filtered[0].ID != "backup/ok" {
		t.Fatalf("filtered finding = %+v", filtered[0])
	}
	if len(filterFindingsByCheck(findings, "no-such-check")) != 0 {
		t.Fatal("unknown check id must filter to nothing")
	}
}

func TestDoctorFormatCarriesContractFields(t *testing.T) {
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	finding := health.Finding{
		ID:          "backup/backup_missing",
		Component:   "backup",
		Scope:       health.ScopeLocal,
		Code:        "backup_missing",
		Status:      health.StatusDown,
		Severity:    health.SeverityCritical,
		Detail:      "no backup found",
		Remediation: health.RemediationRestoreBackup,
		FirstSeen:   now,
		LastSeen:    now,
		CheckedAt:   now,
	}
	text := formatDoctorFinding(&finding)
	for _, want := range []string{"backup/backup_missing", "critical", "remediation: restore-backup", "first_seen:", "last_seen:", "checked_at:"} {
		if !strings.Contains(text, want) {
			t.Errorf("doctor output missing %q:\n%s", want, text)
		}
	}
}

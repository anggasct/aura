package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/capability"
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
// appear. Process-resource evidence is pinned so concurrent test load on
// the host cannot turn the available-path assertion flaky.
func newStatusCmdForTest(t *testing.T, negotiate func() (sandbox.Primitives, error), healthyStorage bool) *cobra.Command {
	t.Helper()
	original := processProbeFn
	processProbeFn = func() (health.ProcessStatus, bool) {
		return health.ProcessStatus{FDsOpen: 10, FDsLimit: 4096, MemoryUsedBytes: 1 << 20, MemoryLimitBytes: 1 << 30, MemoryLimitKnown: true}, true
	}
	t.Cleanup(func() { processProbeFn = original })
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
		return runStatus(c, &cfg, capability.Report{}, negotiate, true, false)
	}
	return cmd
}

// pinProcessProbe replaces host process-resource evidence with a benign
// fixed status so concurrent suite load cannot make finding-set assertions
// flaky.
func pinProcessProbe(t *testing.T) {
	t.Helper()
	original := processProbeFn
	processProbeFn = func() (health.ProcessStatus, bool) {
		return health.ProcessStatus{FDsOpen: 10, FDsLimit: 4096, MemoryUsedBytes: 1 << 20, MemoryLimitBytes: 1 << 30, MemoryLimitKnown: true}, true
	}
	t.Cleanup(func() { processProbeFn = original })
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
	pinProcessProbe(t)
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
	pinProcessProbe(t)
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
	if err := runStatus(cmd, &cfg, capability.Report{}, negotiate, true, true); err != nil {
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
	if len(decoded.Findings) != 8 {
		t.Fatalf("findings = %d, want 8 (migration, backup, storage intake, disk, sandbox, capability, process, provider): %v", len(decoded.Findings), decoded.Findings)
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
	registry, err := buildHealthRegistry(&cfg, nil, negotiate)
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

// The load path must feed the real builtin capability registry into the
// status surface: a fresh configuration yields the shipped capability
// statuses, never an empty report that claims consistency without data.
func TestStatusUsesBuiltinCapabilityRegistry(t *testing.T) {
	// Point the default-config resolution at an isolated home so the load
	// generates a fresh configuration instead of reading this user's.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AURA_CONFIG", "")
	t.Setenv("AURA_TOOLS_WORKSPACE", t.TempDir())
	loaded, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	statuses := mapCapabilityStatuses(loaded.CapabilityReport)
	names := make([]string, 0, len(statuses))
	for _, s := range statuses {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	want := []string{"exec-linux", "provider-search", "public-web", "workspace-read", "workspace-write"}
	if !slices.Equal(names, want) {
		t.Fatalf("capability statuses = %v, want %v", names, want)
	}
	for _, s := range statuses {
		if s.Compiled || s.Enabled {
			t.Fatalf("default build reports %q as compiled=%v enabled=%v, want absent and disabled", s.Name, s.Compiled, s.Enabled)
		}
	}
}

// A capability the configuration enabled but whose host dependency is
// missing reaches the status output with the stable finding code and the
// degraded exit contract.
func TestStatusReportsEnabledCapabilityMissingDependency(t *testing.T) {
	pinProcessProbe(t)
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	build, err := capability.ParseBuild("exec-linux", "exec-linux", "linux")
	if err != nil {
		t.Fatal(err)
	}
	report, _ := registry.Resolve(build, []string{"exec-linux"}, capability.Dependencies{})
	dataRoot := t.TempDir()
	seedHealthyStorage(t, dataRoot)
	cfg := config.Default()
	cfg.Storage.Path = dataRoot
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"primary": {Protocol: "anthropic", Model: "claude-sonnet-4"},
	}
	cmd := &cobra.Command{Use: "status"}
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetContext(t.Context())
	err = runStatus(cmd, &cfg, report, func() (sandbox.Primitives, error) {
		return sandbox.Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}, nil
	}, true, false)
	var exitErr *exitCodeError
	if !errors.As(err, &exitErr) {
		t.Fatalf("runStatus error = %v, want exit code error", err)
	}
	if exitErr.code != exitCritical {
		t.Fatalf("exit code = %d, want %d for a down capability finding", exitErr.code, exitCritical)
	}
	if output := out.String(); !strings.Contains(output, "capability/exec-linux") || !strings.Contains(output, "missing dependency: process-containment") {
		t.Fatalf("status output missing the capability finding:\n%s", output)
	}
}

// A capability enabled in configuration but absent from the artifact is
// reported through the status surface with the not-compiled code.
func TestStatusReportsEnabledCapabilityNotCompiled(t *testing.T) {
	pinProcessProbe(t)
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	build, err := capability.ParseBuild("core", "", "linux")
	if err != nil {
		t.Fatal(err)
	}
	report, _ := registry.Resolve(build, []string{"public-web"}, nil)
	dataRoot := t.TempDir()
	seedHealthyStorage(t, dataRoot)
	cfg := config.Default()
	cfg.Storage.Path = dataRoot
	cfg.Models.Definitions = map[string]config.ModelDefinition{
		"primary": {Protocol: "anthropic", Model: "claude-sonnet-4"},
	}
	cmd := &cobra.Command{Use: "status"}
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetContext(t.Context())
	err = runStatus(cmd, &cfg, report, func() (sandbox.Primitives, error) {
		return sandbox.Primitives{UserNamespace: true, Seccomp: true, CgroupV2: true, Landlock: true, ProcessGroups: true}, nil
	}, true, false)
	var exitErr *exitCodeError
	if !errors.As(err, &exitErr) {
		t.Fatalf("runStatus error = %v, want exit code error", err)
	}
	if exitErr.code != exitCritical {
		t.Fatalf("exit code = %d, want %d for a down capability finding", exitErr.code, exitCritical)
	}
	if output := out.String(); !strings.Contains(output, "capability/public-web") || !strings.Contains(output, "capability_not_compiled") {
		t.Fatalf("status output missing the not-compiled finding:\n%s", output)
	}
}

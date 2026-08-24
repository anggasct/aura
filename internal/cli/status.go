package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/store"
)

// Diagnostics exit codes: 0 healthy, 1 degraded/warning, 2 critical, 3
// command/config/connection error. These are the status and doctor contract;
// scripted callers rely on them.
const (
	exitHealthy   = 0
	exitDegraded  = 1
	exitCritical  = 2
	exitCommand   = 3
	liveProbeWait = time.Second
)

// newStatusCmd builds the status command. The negotiator is injected so tests
// can fix the reported surface without depending on the host the suite runs
// on; the composition root passes sandbox.Negotiate.
func newStatusCmd(gf *globalFlags, negotiate func() (sandbox.Primitives, error)) *cobra.Command {
	var asJSON bool
	var offline bool
	cmd := &cobra.Command{
		Use:           "status",
		Short:         "Report system and sandbox health",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			loaded, err := config.Load(gf.configPath)
			if err != nil {
				return &exitCodeError{code: exitCommand, err: err}
			}
			return runStatus(cmd, loaded.Config, negotiate, offline, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable output")
	cmd.Flags().BoolVar(&offline, "offline", false, "skip the live process probe and use local stores only")
	return cmd
}

func runStatus(cmd *cobra.Command, cfg *config.Config, negotiate func() (sandbox.Primitives, error), offline, asJSON bool) error {
	ctx := cmd.Context()
	registry, err := buildHealthRegistry(cfg, negotiate)
	if err != nil {
		return &exitCodeError{code: exitCommand, err: err}
	}
	findings := registry.Evaluate(ctx)
	live, liveReachable := liveReadiness(ctx, offline, cfg.Health.Listen)
	primitives, negotiateErr := negotiate()
	if asJSON {
		return writeStatusJSON(cmd, primitives, negotiateErr, findings, live, liveReachable)
	}
	return writeStatusText(cmd, primitives, negotiateErr, findings, live, liveReachable)
}

// liveReadiness probes the running process's readiness endpoint. A process
// that is not running is not an error: the command falls back to local
// checks and labels the live surface unreachable.
func liveReadiness(ctx context.Context, offline bool, listen string) (live health.ProbeBody, reachable bool) {
	if offline || listen == "" {
		return health.ProbeBody{}, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, liveProbeWait)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+listen+"/readyz", http.NoBody)
	if err != nil {
		return health.ProbeBody{}, false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return health.ProbeBody{}, false
	}
	defer func() { _ = response.Body.Close() }()
	var body health.ProbeBody
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return health.ProbeBody{}, false
	}
	return body, true
}

// buildHealthRegistry assembles the offline-capable check set from the
// loaded configuration: migration state and backup age from the store opened
// read-only, sandbox primitives from the host, provider configuration and
// capability availability from the load result's inputs. Checks never mutate
// state and never dial a provider.
func buildHealthRegistry(cfg *config.Config, negotiate func() (sandbox.Primitives, error)) (*health.Registry, error) {
	dbPath, _, backupDir, err := storagePaths(cfg)
	if err != nil {
		return nil, err
	}
	checkTimeout := time.Duration(cfg.Health.CheckTimeout)
	return health.NewRegistry(
		health.RegisteredCheck{
			ID:          "migration",
			Checker:     health.MigrationChecker{Versions: func(ctx context.Context) (applied, latest int, err error) { return schemaVersionsReadOnly(ctx, dbPath) }},
			Timeout:     checkTimeout,
			Remediation: health.RemediationRunMigrations,
		},
		health.RegisteredCheck{
			ID:          "backup",
			Checker:     health.BackupChecker{LastBackup: func(ctx context.Context) (time.Time, error) { return lastBackupTime(backupDir) }, MaxAge: time.Duration(cfg.Health.BackupMaxAge)},
			Timeout:     checkTimeout,
			Remediation: health.RemediationRestoreBackup,
		},
		health.RegisteredCheck{
			ID:          "sandbox",
			Checker:     health.SandboxChecker{Support: sandboxSupport(negotiate)},
			Timeout:     checkTimeout,
			Remediation: health.RemediationReviewSandbox,
		},
		health.RegisteredCheck{
			ID:          "provider",
			Checker:     health.ProviderChecker{Probe: func(context.Context) error { return providerConfigured(cfg) }},
			Timeout:     checkTimeout,
			Remediation: health.RemediationConfigureModels,
		},
	)
}

func schemaVersionsReadOnly(ctx context.Context, dbPath string) (applied, latest int, err error) {
	if _, err := os.Stat(dbPath); err != nil {
		return 0, 0, err
	}
	db, err := store.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = db.Close() }()
	return store.SchemaVersions(ctx, db)
}

// lastBackupTime reports the newest backup database mtime under the backup
// directory. Listing is bounded to the top level of the directory: backups
// are written as sibling directories by the storage backup path.
func lastBackupTime(backupDir string) (time.Time, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		return time.Time{}, err
	}
	newest := time.Time{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return time.Time{}, errors.New("no backup directories found")
	}
	return newest, nil
}

func sandboxSupport(negotiate func() (sandbox.Primitives, error)) func() (bool, string) {
	return func() (bool, string) {
		primitives, err := negotiate()
		if err != nil {
			return false, err.Error()
		}
		if missing := sandbox.MissingMandatory(primitives); len(missing) > 0 {
			return false, "missing: " + strings.Join(missing, ", ")
		}
		return true, "all containment primitives available"
	}
}

func providerConfigured(cfg *config.Config) error {
	definition, ok := cfg.Models.Definitions["primary"]
	if !ok || definition.Model == "" {
		return errors.New("no primary model definition")
	}
	return nil
}

func writeStatusText(cmd *cobra.Command, primitives sandbox.Primitives, negotiateErr error, findings []health.Finding, live health.ProbeBody, liveReachable bool) error {
	out := cmd.OutOrStdout()
	sandboxText, sandboxAvailable := formatSandboxStatus(primitives, negotiateErr)
	if _, err := fmt.Fprintln(out, sandboxText); err != nil {
		return err
	}
	if liveReachable {
		liveLine := "live: " + live.Status + " (" + live.Code + ")"
		if _, err := fmt.Fprintln(out, liveLine); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintln(out, "live: unreachable (local checks only)"); err != nil {
			return err
		}
	}
	for i := range findings {
		line := formatFindingLine(&findings[i])
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return statusExit(findings, sandboxAvailable)
}

func writeStatusJSON(cmd *cobra.Command, primitives sandbox.Primitives, negotiateErr error, findings []health.Finding, live health.ProbeBody, liveReachable bool) error {
	_, sandboxAvailable := formatSandboxStatus(primitives, negotiateErr)
	payload := statusJSON{
		Status:   string(health.WorstStatus(findings)),
		Severity: string(health.WorstSeverity(findings)),
		Live: liveStatusJSON{
			Reachable: liveReachable,
			Status:    live.Status,
			Code:      live.Code,
		},
		Findings: findings,
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("status: encode output: %w", err)
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(encoded)); err != nil {
		return err
	}
	return statusExit(findings, sandboxAvailable)
}

type statusJSON struct {
	Status   string           `json:"status"`
	Severity string           `json:"severity"`
	Live     liveStatusJSON   `json:"live"`
	Findings []health.Finding `json:"findings"`
}

type liveStatusJSON struct {
	Reachable bool   `json:"reachable"`
	Status    string `json:"status,omitempty"`
	Code      string `json:"code,omitempty"`
}

// formatFindingLine renders one finding in the stable text contract:
// severity, component, code, detail, and remediation when set.
func formatFindingLine(f *health.Finding) string {
	var b strings.Builder
	b.WriteString(string(f.Severity))
	b.WriteString(" ")
	b.WriteString(f.Component)
	b.WriteString(" ")
	b.WriteString(f.Code)
	if f.Detail != "" {
		b.WriteString(": ")
		b.WriteString(f.Detail)
	}
	if f.Stale {
		b.WriteString(" [stale]")
	}
	if f.Remediation != "" && f.Status != health.StatusUp {
		b.WriteString(" (remediation: " + f.Remediation + ")")
	}
	return b.String()
}

// statusExit maps the worst finding severity to the diagnostics exit code.
// An unavailable sandbox does not change the code by itself: its finding
// already carries degraded severity.
func statusExit(findings []health.Finding, sandboxAvailable bool) error {
	switch health.WorstSeverity(findings) {
	case health.SeverityCritical:
		return &exitCodeError{code: exitCritical, err: errors.New("critical finding present")}
	case health.SeverityWarning:
		return &exitCodeError{code: exitDegraded, err: errors.New("degraded finding present")}
	default:
		if !sandboxAvailable {
			return &exitCodeError{code: exitDegraded, err: errors.New("sandbox unavailable")}
		}
		return nil
	}
}

// formatSandboxStatus renders the exact containment state an operator can act
// on: when a mandatory primitive is absent the line names every one of them,
// matching the Require gate's vocabulary so the status surface and the
// fail-closed gate never disagree.
func formatSandboxStatus(have sandbox.Primitives, negotiateErr error) (string, bool) {
	if negotiateErr != nil {
		return "sandbox: unavailable\nreason: " + negotiateErr.Error(), false
	}
	missing := sandbox.MissingMandatory(have)
	if len(missing) == 0 {
		return "sandbox: available", true
	}
	return "sandbox: unavailable\nmissing: " + strings.Join(missing, ", "), false
}

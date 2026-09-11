package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/capability"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/durable/restate"
	"github.com/anggasct/aura/internal/health"
	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/store"
)

const (
	exitHealthy   = 0
	exitDegraded  = 1
	exitCritical  = 2
	exitCommand   = 3
	liveProbeWait = time.Second
)

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
			return runStatus(cmd, loaded.Config, loaded.CapabilityReport, negotiate, offline, asJSON, processProbe)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable output")
	cmd.Flags().BoolVar(&offline, "offline", false, "skip the live process probe and use local stores only")
	return cmd
}

func runStatus(cmd *cobra.Command, cfg *config.Config, report capability.Report, negotiate func() (sandbox.Primitives, error), offline, asJSON bool, processProbe func() (health.ProcessStatus, bool)) error {
	ctx := cmd.Context()
	registry, err := buildHealthRegistry(cfg, mapCapabilityStatuses(report), negotiate, processProbe)
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

func buildHealthRegistry(cfg *config.Config, capabilities []health.CapabilityStatus, negotiate func() (sandbox.Primitives, error), processProbe func() (health.ProcessStatus, bool), extra ...health.RegisteredCheck) (*health.Registry, error) {
	dbPath, artifactRoot, backupDir, err := storagePaths(cfg)
	if err != nil {
		return nil, err
	}
	checkTimeout := time.Duration(cfg.Health.CheckTimeout)
	checks := make([]health.RegisteredCheck, 0, 9+len(extra))
	checks = append(checks,
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
			ID:          "storage",
			Checker:     health.StorageChecker{Intake: storageIntakeProbe(dbPath)},
			Timeout:     checkTimeout,
			Remediation: health.RemediationRepairStorage,
		},
		health.RegisteredCheck{
			ID: "disk",
			Checker: health.DiskChecker{
				Targets: func(context.Context) []health.FilesystemTarget {
					return diskTargets(dbPath, artifactRoot, backupDir)
				},
				Usage: func(_ context.Context, path string) (health.DiskUsage, error) {
					free, total, inodes, err := diskUsage(path)
					return health.DiskUsage{FreeBytes: free, TotalBytes: total, FreeInodes: inodes}, err
				},
				WarningPercent:  cfg.Health.DiskWarningPercent,
				CriticalPercent: cfg.Health.DiskCriticalPercent,
				FloorBytes:      int64(cfg.Health.DiskCriticalFloorBytes),
			},
			Timeout:     checkTimeout,
			Remediation: health.RemediationRepairStorage,
		},
		health.RegisteredCheck{
			ID:          "capability",
			Checker:     health.CapabilityChecker{Statuses: func(context.Context) []health.CapabilityStatus { return capabilities }},
			Timeout:     checkTimeout,
			Remediation: health.RemediationReviewProfile,
		},
		health.RegisteredCheck{
			ID:          "process",
			Checker:     health.ProcessChecker{Probe: func(context.Context) (health.ProcessStatus, bool) { return processProbe() }},
			Timeout:     checkTimeout,
			Remediation: health.RemediationRelieveLimits,
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
		health.RegisteredCheck{
			ID:          "durable",
			Checker:     health.DurableChecker{State: durableProbeState(cfg)},
			Timeout:     checkTimeout,
			Remediation: health.RemediationReconcileState,
		},
	)
	return health.NewRegistry(append(checks, extra...)...)
}

func durableProbeState(cfg *config.Config) func(context.Context) (health.DurableState, string) {
	return func(ctx context.Context) (health.DurableState, string) {
		durableCfg := cfg.Durable
		if durableCfg == nil || !durableCfg.Enabled {
			return health.DurableDisabled, "durable runtime is disabled"
		}
		if err := restate.CheckExternal(ctx, durableCfg.Endpoint, durableCfg.AdminEndpoint); err != nil {
			return health.DurableUnreachable, "durable runtime is unreachable"
		}
		return health.DurableUp, "durable runtime is reachable"
	}
}

const storageMinFreeBytes = 64 << 20

func storageIntakeProbe(dbPath string) func(context.Context) (health.StorageIntakeState, string) {
	return func(context.Context) (health.StorageIntakeState, string) {
		if _, err := os.Stat(dbPath); err != nil {
			return health.StorageIntakeUnreachable, "storage database is unreachable"
		}
		if err := writableProbe(dbPath); err != nil {
			if classifyOpenError(err) {
				return health.StorageIntakeReadOnly, "storage filesystem is read-only"
			}
			return health.StorageIntakeUnknown, "storage writability could not be confirmed"
		}
		free, err := diskFreeBytes(filepath.Dir(dbPath))
		if err != nil {
			return health.StorageIntakeUnknown, "storage free space could not be determined"
		}
		if free < storageMinFreeBytes {
			return health.StorageIntakeFull, fmt.Sprintf("storage has %d MiB free, below the %d MiB intake floor", free>>20, storageMinFreeBytes>>20)
		}
		return health.StorageIntakeOK, "storage accepts durable writes"
	}
}

func mapCapabilityStatuses(report capability.Report) []health.CapabilityStatus {
	reported := report.Statuses()
	mapped := make([]health.CapabilityStatus, 0, len(reported))
	for _, s := range reported {
		mapped = append(mapped, health.CapabilityStatus{
			Name:              s.Name,
			Compiled:          s.Compiled,
			Available:         s.Available,
			Enabled:           s.Enabled,
			MissingDependency: s.MissingDependency,
			UnavailableReason: s.UnavailableReason,
		})
	}
	return mapped
}

func diskTargets(dbPath, artifactRoot, backupDir string) []health.FilesystemTarget {
	return []health.FilesystemTarget{
		{Name: health.DiskTargetDatabase, Path: existingAncestor(dbPath)},
		{Name: health.DiskTargetArtifacts, Path: existingAncestor(artifactRoot)},
		{Name: health.DiskTargetBackups, Path: existingAncestor(backupDir)},
		{Name: health.DiskTargetTemp, Path: os.TempDir()},
	}
}

func existingAncestor(path string) string {
	current := path
	for range 16 {
		if _, err := os.Stat(current); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return path
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

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/sync"
)

func newSyncCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Synchronize reviewed text assets with a Git remote",
	}
	cmd.AddCommand(
		newSyncStatusCmd(gf),
		newSyncRunCmd(gf),
		newSyncReconcileCmd(gf),
		newSyncDoctorCmd(gf),
	)
	return cmd
}

func loadSyncConfig(gf *globalFlags) (*config.Config, error) {
	result, err := config.Load(gf.configPath)
	if err != nil {
		return nil, err
	}
	if result.Config.Sync == nil {
		return nil, errors.New("sync is not configured")
	}
	return result.Config, nil
}

func syncGateLine(ctx context.Context, cfg *config.Sync) (available bool, line string) {
	outcome := sync.CheckGate(ctx, cfg)
	if outcome.Available {
		return true, "gate: enabled"
	}
	reason := strings.TrimSpace(outcome.Reason)
	if reason == "" {
		reason = "refused"
	}
	if !cfg.Enabled {
		return false, "gate: disabled"
	}
	return false, "gate: unavailable: " + reason
}

func formatRef(ref string) string {
	if strings.TrimSpace(ref) == "" {
		return "-"
	}
	return ref
}

func formatSyncTime(at time.Time) string {
	if at.IsZero() {
		return "-"
	}
	return at.UTC().Format(time.RFC3339)
}

func newSyncStatusCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show sync worker state and last verified refs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadSyncConfig(gf)
			if err != nil {
				return err
			}
			available, _ := syncGateLine(cmd.Context(), cfg.Sync)
			state := sync.NewStateStore().Load()
			status := string(state.State)
			if !available {
				status = string(sync.WorkerDisabled)
			}
			return writeSyncLines(cmd, []string{
				"state: " + status,
				"local_ref: " + formatRef(state.LocalRef),
				"remote_ref: " + formatRef(state.RemoteRef),
				"last_verified_at: " + formatSyncTime(state.VerifiedAt),
			})
		},
	}
}

func newSyncRunCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Trigger one bounded sync pass",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadSyncConfig(gf)
			if err != nil {
				return err
			}
			if available, _ := syncGateLine(cmd.Context(), cfg.Sync); !available {
				return errors.New("sync_unavailable: sync gate refused")
			}
			return writeSyncLines(cmd, []string{
				"result: unknown",
				"local_ref: -",
				"remote_ref: -",
			})
		},
	}
}

func newSyncReconcileCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile",
		Short: "Reconcile an unknown push or fetch by remote ref",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadSyncConfig(gf)
			if err != nil {
				return err
			}
			if available, _ := syncGateLine(cmd.Context(), cfg.Sync); !available {
				return errors.New("sync_unavailable: sync gate refused")
			}
			state := sync.NewStateStore().Load()
			if state.State != sync.WorkerUnknown {
				return errors.New("sync_no_unknown_state: no unknown dispatch to reconcile")
			}
			return writeSyncLines(cmd, []string{
				"result: unknown",
				"local_ref: " + formatRef(state.LocalRef),
				"remote_ref: " + formatRef(state.RemoteRef),
			})
		},
	}
}

func newSyncDoctorCmd(gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Report sync gate, profile, transport, and manifest health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadSyncConfig(gf)
			if err != nil {
				return err
			}
			_, gate := syncGateLine(cmd.Context(), cfg.Sync)
			profile := "profile: ok"
			transport := "transport: ok"
			if _, err := sync.ResolveTransport(cmd.Context(), cfg.Sync, nil); err != nil {
				transport = "transport: invalid: refused"
			}
			manifest := "manifest: ok"
			if _, err := sync.NormalizeManifest(cfg.Sync.Include); err != nil {
				manifest = "manifest: invalid: refused"
			}
			state := sync.NewStateStore().Load()
			refs := "refs: ok"
			switch state.State {
			case sync.WorkerConflict, sync.WorkerUnknown:
				refs = "refs: " + string(state.State)
			case sync.WorkerDisabled, sync.WorkerIdle, sync.WorkerRunning:
				refs = "refs: ok"
			}
			return writeSyncLines(cmd, []string{gate, profile, transport, manifest, refs})
		},
	}
}

func writeSyncLines(cmd *cobra.Command, lines []string) error {
	out := cmd.OutOrStdout()
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

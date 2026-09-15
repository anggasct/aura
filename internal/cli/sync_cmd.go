package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
		return nil, sync.Errorf(sync.ErrorCodeNotConfigured, "sync is not configured")
	}
	return result.Config, nil
}

func openSyncStateStore(cfg *config.Config) (*sync.StateStore, error) {
	_, dataRoot, _, err := storagePaths(cfg)
	if err != nil {
		return nil, err
	}
	return sync.NewFileStateStore(sync.SyncStatePath(dataRoot))
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

func redactSyncReason(err error) string {
	if err == nil {
		return "refused"
	}
	var syncErr *sync.Error
	if errors.As(err, &syncErr) {
		reason := strings.TrimSpace(syncErr.Detail)
		if reason == "" {
			reason = string(syncErr.Code)
		}
		return reason
	}
	reason := strings.TrimSpace(err.Error())
	if reason == "" {
		return "refused"
	}
	return reason
}

func newSyncIntentID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UTC().UnixNano())
	}
	return "run-" + hex.EncodeToString(raw[:])
}

var syncRunPassFunc = defaultSyncRunPass

func defaultSyncRunPass(ctx context.Context, cfg *config.Config, store *sync.StateStore) (sync.PassOutcome, error) {
	if cfg.Sync == nil {
		return sync.PassOutcome{}, sync.Errorf(sync.ErrorCodeNotConfigured, "sync is not configured")
	}
	//nolint:nilerr // manifest invalid is a domain conflict outcome, not a Go failure; the pass completes with a conflict result.
	if _, err := sync.NormalizeManifest(cfg.Sync.Include); err != nil {
		checkpoint := store.Load()
		return sync.PassOutcome{
			State:     sync.WorkerConflict,
			Conflict:  true,
			LocalRef:  checkpoint.LocalRef,
			RemoteRef: checkpoint.RemoteRef,
			Result:    string(sync.AdvanceConflict),
		}, nil
	}
	//nolint:nilerr // transport shape invalid is a retryable unknown outcome, not a Go failure.
	if err := sync.ValidateTransportShape(cfg.Sync); err != nil {
		checkpoint := store.Load()
		return sync.PassOutcome{
			State:     sync.WorkerUnknown,
			UnknownID: newSyncIntentID(),
			LocalRef:  checkpoint.LocalRef,
			RemoteRef: checkpoint.RemoteRef,
			Result:    string(sync.WorkerUnknown),
			Retryable: true,
		}, nil
	}
	checkpoint := store.Load()
	if checkpoint.State == sync.WorkerConflict {
		return sync.PassOutcome{
			State:     sync.WorkerConflict,
			Conflict:  true,
			LocalRef:  checkpoint.LocalRef,
			RemoteRef: checkpoint.RemoteRef,
			Result:    string(sync.AdvanceConflict),
		}, nil
	}
	if checkpoint.State == sync.WorkerUnknown && strings.TrimSpace(checkpoint.UnknownID) != "" {
		return sync.PassOutcome{
			State:     sync.WorkerUnknown,
			UnknownID: checkpoint.UnknownID,
			LocalRef:  checkpoint.LocalRef,
			RemoteRef: checkpoint.RemoteRef,
			Result:    string(sync.WorkerUnknown),
			Retryable: true,
		}, nil
	}
	if strings.TrimSpace(checkpoint.LocalRef) != "" && strings.TrimSpace(checkpoint.RemoteRef) != "" {
		decision, err := sync.DecideAdvance(ctx, checkpoint.LocalRef, checkpoint.RemoteRef)
		//nolint:nilerr // undecidable refs are a domain conflict outcome, not a Go failure.
		if err != nil {
			return sync.PassOutcome{
				State:     sync.WorkerConflict,
				Conflict:  true,
				LocalRef:  checkpoint.LocalRef,
				RemoteRef: checkpoint.RemoteRef,
				Result:    string(sync.AdvanceConflict),
			}, nil
		}
		switch decision {
		case sync.AdvanceUpToDate:
			return sync.PassOutcome{
				State:     sync.WorkerIdle,
				LocalRef:  checkpoint.LocalRef,
				RemoteRef: checkpoint.RemoteRef,
				Result:    string(sync.AdvanceUpToDate),
			}, nil
		case sync.AdvanceFastForward:
			return sync.PassOutcome{
				State:     sync.WorkerIdle,
				LocalRef:  checkpoint.LocalRef,
				RemoteRef: checkpoint.RemoteRef,
				Result:    "fast_forwarded",
			}, nil
		default:
			return sync.PassOutcome{
				State:     sync.WorkerConflict,
				Conflict:  true,
				LocalRef:  checkpoint.LocalRef,
				RemoteRef: checkpoint.RemoteRef,
				Result:    string(sync.AdvanceConflict),
			}, nil
		}
	}
	return sync.PassOutcome{
		State:     sync.WorkerUnknown,
		UnknownID: newSyncIntentID(),
		Result:    string(sync.WorkerUnknown),
		Retryable: true,
	}, nil
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
			store, err := openSyncStateStore(cfg)
			if err != nil {
				return err
			}
			state := store.Load()
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
				return sync.Errorf(sync.ErrorCodeGateRefused, "sync gate refused")
			}
			store, err := openSyncStateStore(cfg)
			if err != nil {
				return err
			}
			pass := syncRunPassFunc
			if pass == nil {
				pass = defaultSyncRunPass
			}
			worker, err := sync.NewWorker(sync.WorkerConfig{
				Interval: time.Duration(cfg.Sync.Interval),
			}, store, func(ctx context.Context) sync.PassOutcome {
				outcome, err := pass(ctx, cfg, store)
				if err != nil {
					return sync.PassOutcome{
						State:     sync.WorkerUnknown,
						UnknownID: newSyncIntentID(),
						Result:    string(sync.WorkerUnknown),
						Retryable: true,
					}
				}
				return outcome
			})
			if err != nil {
				return err
			}
			outcome := worker.RunOnce(cmd.Context())
			return writeSyncLines(cmd, []string{
				"result: " + formatRef(outcome.Result),
				"local_ref: " + formatRef(outcome.LocalRef),
				"remote_ref: " + formatRef(outcome.RemoteRef),
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
				return sync.Errorf(sync.ErrorCodeGateRefused, "sync gate refused")
			}
			store, err := openSyncStateStore(cfg)
			if err != nil {
				return err
			}
			state := store.Load()
			if state.State != sync.WorkerUnknown {
				return sync.Errorf(sync.ErrorCodeNoUnknownState, "no unknown dispatch to reconcile")
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
			if outcome := sync.CheckProfile(cfg.Sync); !outcome.Available {
				reason := strings.TrimSpace(outcome.Reason)
				if reason == "" {
					reason = "refused"
				}
				profile = "profile: invalid: " + reason
			}
			transport := "transport: ok"
			if err := sync.ValidateTransportShape(cfg.Sync); err != nil {
				transport = "transport: invalid: " + redactSyncReason(err)
			}
			manifest := "manifest: ok"
			if _, err := sync.NormalizeManifest(cfg.Sync.Include); err != nil {
				manifest = "manifest: invalid: " + redactSyncReason(err)
			}
			store, err := openSyncStateStore(cfg)
			if err != nil {
				return err
			}
			state := store.Load()
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

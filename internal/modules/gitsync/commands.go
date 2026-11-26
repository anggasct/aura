package gitsync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

type RunPassFunc func(ctx context.Context, cfg *Config, store *StateStore) (PassOutcome, error)

type CommandEnv struct {
	Config   *Config
	DataRoot string
	Pass     RunPassFunc
}

type Opener func(cmd *cobra.Command, fn func(ctx context.Context, env CommandEnv) error) error

func Commands(open Opener) []*cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Synchronize reviewed text assets with a Git remote",
	}
	cmd.AddCommand(
		newStatusCmd(open),
		newRunCmd(open),
		newReconcileCmd(open),
		newDoctorCmd(open),
	)
	return []*cobra.Command{cmd}
}

func runWithOpener(cmd *cobra.Command, open Opener, fn func(ctx context.Context, env CommandEnv) error) error {
	if open == nil {
		return Errorf(ErrorCodeInvalidArgument, "sync command opener must not be nil")
	}
	return open(cmd, func(ctx context.Context, env CommandEnv) error {
		if env.Config == nil {
			return Errorf(ErrorCodeNotConfigured, "sync is not configured")
		}
		return fn(ctx, env)
	})
}

func openStateStore(dataRoot string) (*StateStore, error) {
	return NewFileStateStore(SyncStatePath(dataRoot))
}

func syncGateLine(ctx context.Context, cfg *Config) (available bool, line string) {
	outcome := CheckGate(ctx, cfg)
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
	var syncErr *fault.Error[ErrorCode]
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

func manifestValid(include []string) bool {
	_, err := NormalizeManifest(include)
	return err == nil
}

func transportShapeValid(cfg *Config) bool {
	return ValidateTransportShape(cfg) == nil
}

func decideAdvanceOrConflict(ctx context.Context, localRef, remoteRef string) AdvanceDecision {
	decision, err := DecideAdvance(ctx, localRef, remoteRef)
	if err != nil {
		return AdvanceConflict
	}
	return decision
}

func defaultSyncRunPass(ctx context.Context, cfg *Config, store *StateStore) (PassOutcome, error) {
	if cfg == nil {
		return PassOutcome{}, Errorf(ErrorCodeNotConfigured, "sync is not configured")
	}
	if !manifestValid(cfg.Include) {
		checkpoint := store.Load()
		return PassOutcome{
			State:     WorkerConflict,
			Conflict:  true,
			LocalRef:  checkpoint.LocalRef,
			RemoteRef: checkpoint.RemoteRef,
			Result:    string(AdvanceConflict),
		}, nil
	}
	if !transportShapeValid(cfg) {
		checkpoint := store.Load()
		return PassOutcome{
			State:     WorkerUnknown,
			UnknownID: newSyncIntentID(),
			LocalRef:  checkpoint.LocalRef,
			RemoteRef: checkpoint.RemoteRef,
			Result:    string(WorkerUnknown),
			Retryable: true,
		}, nil
	}
	checkpoint := store.Load()
	if checkpoint.State == WorkerConflict {
		return PassOutcome{
			State:     WorkerConflict,
			Conflict:  true,
			LocalRef:  checkpoint.LocalRef,
			RemoteRef: checkpoint.RemoteRef,
			Result:    string(AdvanceConflict),
		}, nil
	}
	if checkpoint.State == WorkerUnknown && strings.TrimSpace(checkpoint.UnknownID) != "" {
		return PassOutcome{
			State:     WorkerUnknown,
			UnknownID: checkpoint.UnknownID,
			LocalRef:  checkpoint.LocalRef,
			RemoteRef: checkpoint.RemoteRef,
			Result:    string(WorkerUnknown),
			Retryable: true,
		}, nil
	}
	if strings.TrimSpace(checkpoint.LocalRef) != "" && strings.TrimSpace(checkpoint.RemoteRef) != "" {
		switch decideAdvanceOrConflict(ctx, checkpoint.LocalRef, checkpoint.RemoteRef) {
		case AdvanceUpToDate:
			return PassOutcome{
				State:     WorkerIdle,
				LocalRef:  checkpoint.LocalRef,
				RemoteRef: checkpoint.RemoteRef,
				Result:    string(AdvanceUpToDate),
			}, nil
		case AdvanceFastForward:
			return PassOutcome{
				State:     WorkerIdle,
				LocalRef:  checkpoint.LocalRef,
				RemoteRef: checkpoint.RemoteRef,
				Result:    "fast_forwarded",
			}, nil
		default:
			return PassOutcome{
				State:     WorkerConflict,
				Conflict:  true,
				LocalRef:  checkpoint.LocalRef,
				RemoteRef: checkpoint.RemoteRef,
				Result:    string(AdvanceConflict),
			}, nil
		}
	}
	return PassOutcome{
		State:     WorkerUnknown,
		UnknownID: newSyncIntentID(),
		Result:    string(WorkerUnknown),
		Retryable: true,
	}, nil
}

func newStatusCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show sync worker state and last verified refs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, env CommandEnv) error {
				available, _ := syncGateLine(ctx, env.Config)
				store, err := openStateStore(env.DataRoot)
				if err != nil {
					return err
				}
				state := store.Load()
				status := string(state.State)
				if !available {
					status = string(WorkerDisabled)
				}
				return writeLines(cmd, []string{
					"state: " + status,
					"local_ref: " + formatRef(state.LocalRef),
					"remote_ref: " + formatRef(state.RemoteRef),
					"last_verified_at: " + formatSyncTime(state.VerifiedAt),
				})
			})
		},
	}
}

func newRunCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Trigger one bounded sync pass",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, env CommandEnv) error {
				if available, _ := syncGateLine(ctx, env.Config); !available {
					return Errorf(ErrorCodeGateRefused, "sync gate refused")
				}
				store, err := openStateStore(env.DataRoot)
				if err != nil {
					return err
				}
				pass := env.Pass
				if pass == nil {
					pass = defaultSyncRunPass
				}
				worker, err := NewWorker(WorkerConfig{
					Interval: time.Duration(env.Config.Interval),
				}, store, func(passCtx context.Context) PassOutcome {
					outcome, err := pass(passCtx, env.Config, store)
					if err != nil {
						return PassOutcome{
							State:     WorkerUnknown,
							UnknownID: newSyncIntentID(),
							Result:    string(WorkerUnknown),
							Retryable: true,
						}
					}
					return outcome
				})
				if err != nil {
					return err
				}
				outcome := worker.RunOnce(ctx)
				return writeLines(cmd, []string{
					"result: " + formatRef(outcome.Result),
					"local_ref: " + formatRef(outcome.LocalRef),
					"remote_ref: " + formatRef(outcome.RemoteRef),
				})
			})
		},
	}
}

func newReconcileCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile",
		Short: "Reconcile an unknown push or fetch by remote ref",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, env CommandEnv) error {
				if available, _ := syncGateLine(ctx, env.Config); !available {
					return Errorf(ErrorCodeGateRefused, "sync gate refused")
				}
				store, err := openStateStore(env.DataRoot)
				if err != nil {
					return err
				}
				state := store.Load()
				if state.State != WorkerUnknown {
					return Errorf(ErrorCodeNoUnknownState, "no unknown dispatch to reconcile")
				}
				return writeLines(cmd, []string{
					"result: unknown",
					"local_ref: " + formatRef(state.LocalRef),
					"remote_ref: " + formatRef(state.RemoteRef),
				})
			})
		},
	}
}

func newDoctorCmd(open Opener) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Report sync gate, profile, transport, and manifest health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWithOpener(cmd, open, func(ctx context.Context, env CommandEnv) error {
				_, gate := syncGateLine(ctx, env.Config)
				profile := "profile: ok"
				if outcome := CheckProfile(env.Config); !outcome.Available {
					reason := strings.TrimSpace(outcome.Reason)
					if reason == "" {
						reason = "refused"
					}
					profile = "profile: invalid: " + reason
				}
				transport := "transport: ok"
				if err := ValidateTransportShape(env.Config); err != nil {
					transport = "transport: invalid: " + redactSyncReason(err)
				}
				manifest := "manifest: ok"
				if _, err := NormalizeManifest(env.Config.Include); err != nil {
					manifest = "manifest: invalid: " + redactSyncReason(err)
				}
				store, err := openStateStore(env.DataRoot)
				if err != nil {
					return err
				}
				state := store.Load()
				refs := "refs: ok"
				switch state.State {
				case WorkerConflict, WorkerUnknown:
					refs = "refs: " + string(state.State)
				case WorkerDisabled, WorkerIdle, WorkerRunning:
					refs = "refs: ok"
				}
				return writeLines(cmd, []string{gate, profile, transport, manifest, refs})
			})
		},
	}
}

func writeLines(cmd *cobra.Command, lines []string) error {
	out := cmd.OutOrStdout()
	for _, line := range lines {
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	return nil
}

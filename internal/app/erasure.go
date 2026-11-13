package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/store"
)

const (
	accessActionPrincipalForget = "principal_forget"
	retentionSweepInterval      = 24 * time.Hour
	erasureRunPoll              = 500 * time.Millisecond
	erasureRunBound             = 30 * time.Second
)

func erasureLines(counts *store.ErasureCounts) []string {
	return []string{
		fmt.Sprintf("sessions: %d", counts.Sessions),
		fmt.Sprintf("memory_documents: %d", counts.MemoryDocuments),
		fmt.Sprintf("grants: %d", counts.Grants),
		fmt.Sprintf("standing_grants: %d", counts.StandingGrants),
		fmt.Sprintf("pairing_requests: %d", counts.PairingRequests),
		fmt.Sprintf("usage_links: %d", counts.UsageLinks),
		fmt.Sprintf("email_effects: %d", counts.EmailEffects),
		fmt.Sprintf("workflow_runs: %d", counts.WorkflowRuns),
		fmt.Sprintf("workflow_approvals: %d", counts.WorkflowApprovals),
		fmt.Sprintf("legacy_step_sessions: %d", counts.LegacyStepSessions),
		fmt.Sprintf("live_workflow_runs: %d", counts.LiveWorkflowRuns),
	}
}

type runCanceller interface {
	Cancel(ctx context.Context, runID string) error
}

type erasureRuns struct {
	canceller atomic.Pointer[runCanceller]
	live      func(ctx context.Context, db *sql.DB, principalID string) ([]string, error)
	poll      time.Duration
	bound     time.Duration
}

func (r *erasureRuns) set(canceller runCanceller) {
	if r != nil && canceller != nil {
		r.canceller.Store(&canceller)
	}
}

func (r *erasureRuns) settings() (live func(ctx context.Context, db *sql.DB, principalID string) ([]string, error), poll, bound time.Duration) {
	live, poll, bound = r.live, r.poll, r.bound
	if live == nil {
		live = store.LiveWorkflowRuns
	}
	if poll <= 0 {
		poll = erasureRunPoll
	}
	if bound <= 0 {
		bound = erasureRunBound
	}
	return live, poll, bound
}

func (r *erasureRuns) stop(ctx context.Context, db *sql.DB, principalID string) ([]string, error) {
	if r == nil {
		return nil, nil
	}
	canceller := r.canceller.Load()
	if canceller == nil {
		return nil, nil
	}
	live, poll, bound := r.settings()
	runs, err := live(ctx, db, principalID)
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	for _, id := range runs {
		if err := (*canceller).Cancel(ctx, id); err != nil && !errors.Is(err, durable.ErrUnknownRun) {
			return nil, fmt.Errorf("cancel workflow run %s: %w", id, err)
		}
	}
	deadline := time.NewTimer(bound)
	defer deadline.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		remaining, err := live(ctx, db, principalID)
		if err != nil {
			return runs, err
		}
		if len(remaining) == 0 {
			return runs, nil
		}
		select {
		case <-ctx.Done():
			return runs, ctx.Err()
		case <-deadline.C:
			return runs, nil
		case <-ticker.C:
		}
	}
}

func forgetPrincipal(ctx context.Context, db *sql.DB, runs *erasureRuns, principalID string, apply bool, opts store.ErasureOptions) (cancelled []string, counts store.ErasureCounts, err error) {
	if apply {
		if cancelled, err = runs.stop(ctx, db, principalID); err != nil {
			return nil, counts, err
		}
	}
	counts, err = store.ErasePrincipal(ctx, db, principalID, apply, opts)
	return cancelled, counts, err
}

type principalEraser struct {
	db           *sql.DB
	artifactRoot string
	now          func() time.Time
	runs         *erasureRuns
}

func (e *principalEraser) Forget(ctx context.Context, principalID string) error {
	if _, _, err := forgetPrincipal(ctx, e.db, e.runs, principalID, true, store.ErasureOptions{ArtifactRoot: e.artifactRoot}); err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeErasureBlocked {
			return fmt.Errorf("%w: %w", access.ErrForgetRunsStopping, err)
		}
		return err
	}
	channel, _, _ := strings.Cut(principalID, ":")
	return recordAccessAction(ctx, e.db, &accessAction{Action: accessActionPrincipalForget, Principal: principalID, Channel: channel}, "forgetme-"+principalID, e.now().UTC())
}

func newPrincipalForgetCmd(gf *globalFlags) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "forget <id>",
		Short: "Erase a trusted or public principal's sessions, memory, grants, pairing rows and workflow runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !apply {
				return withStorage(cmd, gf, true, func(ctx context.Context, _ *slog.Logger, cfg *config.Config, db *sql.DB) error {
					return forgetCommand(ctx, cmd, cfg, db, nil, args[0], false)
				})
			}
			result, err := config.Load(gf.configPath)
			if err != nil {
				return err
			}
			return withApp(cmd, &commandInput{result: &result, parts: []Part{PartWorkflows}}, func(ctx context.Context, app *App) error {
				runs := &erasureRuns{}
				if app.interpreter != nil {
					runs.set(app.interpreter)
				}
				return forgetCommand(ctx, cmd, result.Config, app.db, runs, args[0], true)
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "erase instead of printing what would be erased")
	return cmd
}

func forgetCommand(ctx context.Context, cmd *cobra.Command, cfg *config.Config, db *sql.DB, runs *erasureRuns, principalID string, apply bool) error {
	channel, externalID, err := principal.ParseIdentity(principalID)
	if err != nil {
		return &usageError{err: errors.New("principal must be a channel identity such as discord:<user id>")}
	}
	owners, err := ownerResolver(cfg)
	if err != nil {
		return err
	}
	if owners.IsOwnerIdentity(channel, externalID) {
		return &usageError{err: errors.New("the owner is not a principal; use aura memory forget for owner data")}
	}
	_, artifactRoot, _, err := storagePaths(cfg)
	if err != nil {
		return err
	}
	cancelled, counts, err := forgetPrincipal(ctx, db, runs, principalID, apply, store.ErasureOptions{ArtifactRoot: artifactRoot})
	if err != nil {
		return err
	}
	lines := erasureLines(&counts)
	if len(cancelled) > 0 {
		lines = append([]string{"cancelled_runs: " + strings.Join(cancelled, ", ")}, lines...)
	}
	if !apply {
		return writeChildLines(cmd, append(lines, "dry run: pass --apply to erase "+principalID))
	}
	if err := recordAccessAction(ctx, db, &accessAction{Action: accessActionPrincipalForget, Principal: principalID, Channel: channel}, "forget-"+principalID, time.Now().UTC()); err != nil {
		return err
	}
	return writeChildLines(cmd, append(lines, "erased "+principalID))
}

func sweepRetention(ctx context.Context, cfg *config.Config, db *sql.DB, artifactRoot string, now time.Time) (map[principal.Class]int, error) {
	swept := map[principal.Class]int{}
	if cfg.Access == nil {
		return swept, nil
	}
	ttl := map[principal.Class]time.Duration{
		principal.ClassTrusted: time.Duration(cfg.Access.Retention.Trusted),
		principal.ClassPublic:  time.Duration(cfg.Access.Retention.Public),
	}
	summaries, err := collectPrincipals(ctx, cfg, db)
	if err != nil {
		return swept, err
	}
	for _, summary := range summaries {
		limit, ok := ttl[summary.class]
		if !ok || summary.lastActive.IsZero() || now.Sub(summary.lastActive) <= limit {
			continue
		}
		if _, err := store.ErasePrincipal(ctx, db, summary.id, true, store.ErasureOptions{KeepGrants: true, ArtifactRoot: artifactRoot}); err != nil {
			if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeErasureBlocked {
				continue
			}
			return swept, err
		}
		swept[summary.class]++
	}
	return swept, nil
}

func runRetentionSweeps(ctx context.Context, cfg *config.Config, db *sql.DB, artifactRoot string, logger *slog.Logger) {
	sweep := func() {
		swept, err := sweepRetention(ctx, cfg, db, artifactRoot, time.Now().UTC())
		if err != nil {
			logger.WarnContext(ctx, "retention sweep failed", "component", "access", "error", err)
			return
		}
		logger.InfoContext(ctx, "retention sweep", "component", "access", "trusted", swept[principal.ClassTrusted], "public", swept[principal.ClassPublic])
	}
	sweep()
	ticker := time.NewTicker(retentionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

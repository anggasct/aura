package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/child"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/durable"
	"github.com/anggasct/aura/internal/store"
)

type childRegistry struct {
	db *sql.DB
}

func newChildRegistry(db *sql.DB) *childRegistry {
	return &childRegistry{db: db}
}

func (r *childRegistry) Spawn(ctx context.Context, spec *child.Spec, now time.Time) (child.Spawn, bool, error) {
	if spec == nil {
		return child.Spawn{}, false, errors.New("cli: child spec must not be nil")
	}
	grants, err := json.Marshal(spec.RequestedGrants)
	if err != nil {
		return child.Spawn{}, false, fmt.Errorf("cli: encode child grants: %w", err)
	}
	budget, err := json.Marshal(spec.Budget)
	if err != nil {
		return child.Spawn{}, false, fmt.Errorf("cli: encode child budget: %w", err)
	}
	children := store.NewChildStore(r.db)
	existing, found, err := children.GetRunByInvocation(ctx, spec.ParentInvocation, spec.IdempotencyKey)
	if err != nil {
		return child.Spawn{}, false, err
	}
	if found {
		if existing.ContextDigest != spec.ContextDigest {
			return child.Spawn{}, false, fmt.Errorf("cli: child invocation conflicts with an existing child: %w", childConflictSentinel())
		}
		return childSpawnFromStore(&existing), false, nil
	}
	deadline := now.Add(spec.Budget.Timeout)
	parentDepth, err := r.Depth(ctx, spec.ParentSessionID)
	if err != nil {
		return child.Spawn{}, false, err
	}
	if parentDepth+1 > 1 {
		return child.Spawn{}, false, child.Errorf(child.ErrorCodeChildDepthExceeded, "child depth exceeds the maximum of one")
	}
	run := &store.ChildRun{
		ID: spec.ID, IdempotencyKey: spec.IdempotencyKey,
		ParentSessionID: spec.ParentSessionID, ParentTurnID: spec.ParentTurnID,
		ParentInvocation: spec.ParentInvocation, ChildSessionID: spec.ChildSessionID,
		DurableKey: child.DurableChildKey(spec.ID), ContextDigest: spec.ContextDigest,
		GrantsJSON: string(grants), BudgetJSON: string(budget),
		State: child.StatusQueued, Deadline: deadline, CreatedAt: now, UpdatedAt: now,
	}
	if err := children.InsertRun(ctx, run); err != nil {
		if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeChildConflict {
			existing, found, readErr := children.GetRunByInvocation(ctx, spec.ParentInvocation, spec.IdempotencyKey)
			if readErr != nil {
				return child.Spawn{}, false, readErr
			}
			if found && existing.ContextDigest == spec.ContextDigest {
				return childSpawnFromStore(&existing), false, nil
			}
			return child.Spawn{}, false, fmt.Errorf("cli: child run conflicts under concurrency: %w", childConflictSentinel())
		}
		return child.Spawn{}, false, err
	}
	return child.Spawn{
		ID: run.ID, SessionID: run.ChildSessionID, Grants: spec.RequestedGrants, DurableKey: run.DurableKey,
		ContextDigest: run.ContextDigest, Deadline: deadline, CreatedAt: now,
	}, true, nil
}

func (r *childRegistry) Get(ctx context.Context, id string) (child.Spawn, bool, error) {
	children := store.NewChildStore(r.db)
	run, found, err := children.GetRun(ctx, id)
	if err != nil || !found {
		return child.Spawn{}, found, err
	}
	return childSpawnFromStore(&run), true, nil
}

func (r *childRegistry) Depth(ctx context.Context, sessionID string) (int, error) {
	if strings.TrimSpace(sessionID) == "" {
		return 0, errors.New("cli: child session must not be empty")
	}
	if r.db == nil {
		return 0, errors.New("cli: child store must not be nil")
	}
	var one int
	if err := r.db.QueryRowContext(ctx, `SELECT 1 FROM child_run WHERE child_session_id = ? LIMIT 1`, sessionID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return 1, nil
}

func childSpawnFromStore(run *store.ChildRun) child.Spawn {
	var grants []child.Grant
	if err := json.Unmarshal([]byte(run.GrantsJSON), &grants); err != nil {
		grants = nil
	}
	return child.Spawn{
		ID: run.ID, SessionID: run.ChildSessionID, Grants: grants, DurableKey: run.DurableKey,
		ContextDigest: run.ContextDigest, Deadline: run.Deadline, CreatedAt: run.CreatedAt,
	}
}

func childConflictSentinel() error {
	return child.Errorf(child.ErrorCodeChildConflict, "child run conflicts")
}

type childCanceller struct {
	registry *childRegistry
	runs     child.Canceller
}

func (c *childCanceller) Cancel(ctx context.Context, id string) (string, error) {
	service, err := child.NewCancelService(c.registry, c.runs)
	if err != nil {
		return "", err
	}
	return service.Cancel(ctx, id, time.Now().UTC())
}

func (r *childRegistry) RunState(ctx context.Context, id string) (string, error) {
	children := store.NewChildStore(r.db)
	run, found, err := children.GetRun(ctx, id)
	if err != nil {
		return "", err
	}
	if !found {
		return "", child.Errorf(child.ErrorCodeChildNotFound, "child is not found")
	}
	return run.State, nil
}

func (r *childRegistry) SetChildState(ctx context.Context, id, state string, now time.Time) error {
	return store.NewChildStore(r.db).SetState(ctx, id, state, now)
}

func (r *childRegistry) ActiveForParent(ctx context.Context, parentSessionID string) ([]child.Spawn, error) {
	runs, err := store.NewChildStore(r.db).ActiveForParent(ctx, parentSessionID)
	if err != nil {
		return nil, err
	}
	spawns := make([]child.Spawn, 0, len(runs))
	for i := range runs {
		spawns = append(spawns, childSpawnFromStore(&runs[i]))
	}
	return spawns, nil
}

func openChildCanceller(cmd *cobra.Command, gf *globalFlags) (*childCanceller, func(), error) {
	result, err := config.Load(gf.configPath)
	if err != nil {
		return nil, nil, err
	}
	if result.Config.Children == nil || !result.Config.Children.Enabled {
		return nil, nil, errors.New("subagent runner is not enabled")
	}
	db, err := openStorage(cmd.Context(), result.Config)
	if err != nil {
		return nil, nil, err
	}
	closer := func() { _ = db.Close() }
	runtime, err := durableRuntimeForConfig(result.Config, nil)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return &childCanceller{registry: newChildRegistry(db), runs: &durableChildRuns{runtime: runtime}}, closer, nil
}

type durableChildRuns struct {
	runtime durable.Runtime
}

func (d *durableChildRuns) CancelRun(ctx context.Context, durableKey string) error {
	if d == nil || d.runtime == nil {
		return errors.New("cli: durable runtime must not be nil")
	}
	if durableKey == "" {
		return errors.New("cli: durable key must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.runtime.Cancel(ctx, durable.RunRef{Key: durableKey})
}

func newChildrenCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "children",
		Short: "Inspect and manage durable child agent runs",
	}
	cmd.AddCommand(
		newChildrenListCmd(gf),
		newChildrenShowCmd(gf),
		newChildrenCancelCmd(gf),
	)
	return cmd
}

type childHandlerRuns struct {
	store store.ChildStore
}

func (r *childHandlerRuns) GetRun(ctx context.Context, id string) (child.HandlerRun, bool, error) {
	run, found, err := r.store.GetRun(ctx, id)
	if err != nil || !found {
		return child.HandlerRun{}, found, err
	}
	return child.HandlerRun{ID: run.ID, SessionID: run.ChildSessionID, State: run.State, Deadline: run.Deadline, GrantsJSON: run.GrantsJSON, ContextDigest: run.ContextDigest}, true, nil
}

func (r *childHandlerRuns) SetState(ctx context.Context, id, state string, now time.Time) error {
	return r.store.SetState(ctx, id, state, now)
}

func (r *childHandlerRuns) SetResult(_ context.Context, _ string, _ child.Result, _ time.Time) error {
	return nil
}

func registerChildHandler(target any, handler *child.Handler) error {
	if handler == nil {
		return errors.New("child handler must not be nil")
	}
	type registrar interface {
		RegisterHandler(string, durable.Handler)
	}
	ifTodo, ok := target.(registrar)
	if !ok || ifTodo == nil {
		return errors.New("child handler target does not accept handlers")
	}
	ifTodo.RegisterHandler(child.HandlerName, func(ctx context.Context, inv durable.Invocation) error {
		return handler.HandleDurable(ctx, inv, time.Now().UTC)
	})
	return nil
}

func buildChildHandler(db *sql.DB) (*child.Handler, error) {
	handler, err := child.NewHandlerWithLedger(&childHandlerRuns{store: store.NewChildStore(db)}, childSessionRunner{}, child.NewLedger(nil, 0))
	if err != nil {
		return nil, err
	}
	return handler, nil
}

type childSessionRunner struct{}

func (childSessionRunner) RunSession(ctx context.Context, sessionID string, deadline time.Time) (child.Result, error) {
	if sessionID == "" {
		return child.Result{}, errors.New("cli: child session must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return child.Result{}, err
	}
	return child.Result{Status: "completed", CompletedAt: deadline}, nil
}

func (childSessionRunner) CancelSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("cli: child session must not be empty")
	}
	return ctx.Err()
}

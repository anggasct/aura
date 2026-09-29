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
	_ = d.runtime.Signal(ctx, durable.RunRef{Key: durableKey}, "child-cancel/"+durableKey, []byte("cancel"))
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
	return child.HandlerRun{ID: run.ID, SessionID: run.ChildSessionID, State: run.State, Deadline: run.Deadline, GrantsJSON: run.GrantsJSON, ContextDigest: run.ContextDigest, DurableKey: run.DurableKey, ParentInvocation: run.ParentInvocation}, true, nil
}

func (r *childHandlerRuns) SetState(ctx context.Context, id, state string, now time.Time) error {
	return r.store.SetState(ctx, id, state, now)
}

func (r *childHandlerRuns) SetResult(ctx context.Context, id string, result *child.Result, now time.Time) error {
	if r.store == nil {
		return errors.New("cli: child store must not be nil")
	}
	if result == nil {
		return errors.New("cli: child result must not be nil")
	}
	run, found, err := r.store.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return child.Errorf(child.ErrorCodeChildNotFound, "child is not found")
	}
	artifactsJSON := "[]"
	if len(result.Artifacts) > 0 {
		encoded, err := json.Marshal(result.Artifacts)
		if err != nil {
			return fmt.Errorf("cli: encode child result artifacts: %w", err)
		}
		artifactsJSON = string(encoded)
	}
	completedAt := result.CompletedAt
	if completedAt.IsZero() {
		completedAt = now.UTC()
	}
	childID := result.ChildID
	if strings.TrimSpace(childID) == "" {
		childID = run.ID
	}
	sessionID := result.SessionID
	if strings.TrimSpace(sessionID) == "" {
		sessionID = run.ChildSessionID
	}
	digest := result.ContextDigest
	if strings.TrimSpace(digest) == "" {
		digest = run.ContextDigest
	}
	durableKey := result.DurableKey
	if strings.TrimSpace(durableKey) == "" {
		durableKey = run.DurableKey
	}
	sourceRange := result.SourceRange
	if strings.TrimSpace(sourceRange) == "" {
		sourceRange = run.ParentInvocation
	}
	model := result.Model
	if strings.TrimSpace(model) == "" {
		model = "child-default"
	}
	promptVersion := result.PromptVersion
	if strings.TrimSpace(promptVersion) == "" {
		promptVersion = "v1"
	}
	trust := result.Trust
	if strings.TrimSpace(trust) == "" {
		trust = "derived_untrusted"
	}
	provenance := "child=" + childID + " session=" + sessionID + " digest=" + digest + " durable=" + durableKey
	return r.store.SetResult(ctx, id, &store.ChildResult{
		Status: result.Status, Output: result.Output, ArtifactsJSON: artifactsJSON,
		TokensUsed: result.TokensUsed, CostMicros: result.CostMicros,
		CompletedAt: completedAt, Provenance: provenance,
		ChildID: childID, SessionID: sessionID, ContextDigest: digest, DurableKey: durableKey,
		SourceRange: sourceRange, Model: model, PromptVersion: promptVersion, Trust: trust,
	}, now)
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
	handler, err := child.NewHandlerWithLedger(&childHandlerRuns{store: store.NewChildStore(db)}, childSessionRunner{db: db}, child.NewLedger(nil, 0))
	if err != nil {
		return nil, err
	}
	return handler, nil
}

type childSessionRunner struct {
	db *sql.DB
}

func (r childSessionRunner) RunSession(ctx context.Context, sessionID string, deadline time.Time) (child.Result, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return child.Result{}, errors.New("cli: child session must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return child.Result{}, err
	}
	now := time.Now().UTC()
	if !deadline.IsZero() && !now.Before(deadline.UTC()) {
		return child.Result{Status: "deadline", SessionID: sessionID, Model: "child-default", PromptVersion: "v1", Trust: "derived_untrusted", CompletedAt: now}, nil
	}
	projection := r.loadProjection(ctx, sessionID)
	summary := buildChildSummary(sessionID, projection)
	if len(summary) > 8192 {
		summary = summary[:8192]
	}
	if scope, ok := durable.TurnScopeFrom(ctx); ok && scope != nil {
		if _, err := scope.Invocation().RunAction(ctx, "child-model/"+sessionID, func(context.Context) ([]byte, error) {
			return json.Marshal(map[string]string{"summary": summary})
		}); err != nil {
			return child.Result{}, err
		}
		if _, err := scope.Invocation().RunAction(ctx, "child-tool/"+sessionID, func(context.Context) ([]byte, error) {
			return json.Marshal(map[string]any{"grants": projection.grantNames()})
		}); err != nil {
			return child.Result{}, err
		}
		if _, err := scope.Invocation().RunAction(ctx, "child-effect/"+sessionID, func(context.Context) ([]byte, error) {
			return json.Marshal(map[string]int64{"tokens": childTokensFor(summary), "cost": childCostFor(summary)})
		}); err != nil {
			return child.Result{}, err
		}
	}
	tokens := childTokensFor(summary)
	cost := childCostFor(summary)
	if projection.hasBudget {
		if projection.maxTokens > 0 && tokens > projection.maxTokens {
			return child.Result{}, child.Errorf(child.ErrorCodeBudgetExceeded, "child budget is exceeded")
		}
		if projection.maxCost > 0 && cost > projection.maxCost {
			return child.Result{}, child.Errorf(child.ErrorCodeBudgetExceeded, "child budget is exceeded")
		}
	}
	result := child.Result{
		Status: "completed", Output: summary, SessionID: sessionID,
		Model: "child-default", PromptVersion: "v1", Trust: "derived_untrusted",
		TokensUsed: tokens, CostMicros: cost, CompletedAt: time.Now().UTC(),
	}
	projection.applyTo(&result)
	return result, nil
}

type childProjection struct {
	childID     string
	digest      string
	durableKey  string
	sourceRange string
	grants      []child.Grant
	hasBudget   bool
	maxTokens   int64
	maxCost     int64
}

func (p *childProjection) grantNames() []string {
	names := make([]string, 0, len(p.grants))
	for _, grant := range p.grants {
		name := strings.TrimSpace(grant.Capability)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	return names
}

func (p *childProjection) applyTo(result *child.Result) {
	if result == nil {
		return
	}
	if strings.TrimSpace(result.ChildID) == "" && strings.TrimSpace(p.childID) != "" {
		result.ChildID = p.childID
	}
	if strings.TrimSpace(result.ContextDigest) == "" && strings.TrimSpace(p.digest) != "" {
		result.ContextDigest = p.digest
	}
	if strings.TrimSpace(result.DurableKey) == "" && strings.TrimSpace(p.durableKey) != "" {
		result.DurableKey = p.durableKey
	}
	if strings.TrimSpace(result.SourceRange) == "" && strings.TrimSpace(p.sourceRange) != "" {
		result.SourceRange = p.sourceRange
	}
}

func (r childSessionRunner) loadProjection(ctx context.Context, sessionID string) *childProjection {
	projection := &childProjection{}
	if r.db == nil {
		return projection
	}
	var id, parentInvocation, durableKey, digest, grantsJSON, budgetJSON string
	err := r.db.QueryRowContext(ctx, `SELECT id, parent_invocation_id, durable_key, context_digest, grants_json, budget_json FROM child_run WHERE child_session_id = ? LIMIT 1`, sessionID).Scan(&id, &parentInvocation, &durableKey, &digest, &grantsJSON, &budgetJSON)
	if err != nil {
		return projection
	}
	projection.childID = id
	projection.digest = digest
	projection.durableKey = durableKey
	projection.sourceRange = parentInvocation
	if json.Valid([]byte(grantsJSON)) {
		var grants []child.Grant
		if err := json.Unmarshal([]byte(grantsJSON), &grants); err == nil {
			valid := grants[:0]
			for _, grant := range grants {
				if strings.EqualFold(strings.TrimSpace(grant.Capability), "spawn_child") {
					continue
				}
				valid = append(valid, grant)
			}
			projection.grants = valid
		}
	}
	if json.Valid([]byte(budgetJSON)) {
		var budget child.Budget
		if err := json.Unmarshal([]byte(budgetJSON), &budget); err == nil {
			projection.hasBudget = true
			projection.maxTokens = budget.MaxTokens
			projection.maxCost = budget.MaxCost
		}
	}
	return projection
}

func buildChildSummary(sessionID string, projection *childProjection) string {
	sessionID = strings.TrimSpace(sessionID)
	if projection == nil || strings.TrimSpace(projection.digest) == "" {
		return "child " + sessionID + " completed"
	}
	digest := strings.TrimSpace(projection.digest)
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return "child " + sessionID + " digest " + digest + " grants " + strings.Join(projection.grantNames(), ",") + " completed"
}

func childTokensFor(summary string) int64 {
	return int64(len(summary)+3)/4 + 1
}

func childCostFor(summary string) int64 {
	return childTokensFor(summary) * 7
}

func (childSessionRunner) CancelSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("cli: child session must not be empty")
	}
	return ctx.Err()
}

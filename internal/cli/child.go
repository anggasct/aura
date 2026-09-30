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
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/adk"
	"github.com/anggasct/aura/internal/runtime/ingress"
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
	sessions := store.NewSessionService(r.db)
	if err := sessions.Create(ctx, &store.Session{ID: spec.ChildSessionID, OwnerID: spec.OwnerID, CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
		if code, ok := store.CodeOf(err); !ok || code != store.ErrorCodeSessionIDConflict {
			return child.Spawn{}, false, err
		}
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
	durableRT, err := durableRuntimeForConfig(result.Config, nil)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return &childCanceller{registry: newChildRegistry(db), runs: &durableChildRuns{runtime: durableRT}}, closer, nil
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

func buildChildHandler(db *sql.DB, ledger child.BudgetLedger, signaler child.Signaler, exec *runtimeadk.ADKExecutor, modelName string) (*child.Handler, error) {
	if ledger == nil {
		ledger = child.NewLedger(nil, 0)
	}
	handler, err := child.NewHandlerWithLedger(&childHandlerRuns{store: store.NewChildStore(db)}, childSessionRunner{db: db, exec: exec, modelName: modelName}, ledger)
	if err != nil {
		return nil, err
	}
	handler.SetSignaler(signaler)
	return handler, nil
}

type runtimeSignaler struct {
	runtime durable.Runtime
}

func (s *runtimeSignaler) SignalChild(ctx context.Context, durableKey, signal string, payload []byte) error {
	if s == nil || s.runtime == nil {
		return errors.New("cli: durable runtime must not be nil")
	}
	if strings.TrimSpace(durableKey) == "" || strings.TrimSpace(signal) == "" {
		return errors.New("cli: durable key and signal must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.runtime.Signal(ctx, durable.RunRef{Key: strings.TrimSpace(durableKey)}, strings.TrimSpace(signal), payload)
}

type childSessionRunner struct {
	db        *sql.DB
	exec      *runtimeadk.ADKExecutor
	modelName string
}

func (r childSessionRunner) RunSession(ctx context.Context, req child.RunRequest) (child.Result, error) {
	sessionID := strings.TrimSpace(req.SessionID)
	deadline := req.Deadline
	task := strings.TrimSpace(req.Task)
	if sessionID == "" {
		return child.Result{}, errors.New("cli: child session must not be empty")
	}
	now := time.Now().UTC()
	if !deadline.IsZero() && !now.Before(deadline.UTC()) {
		return child.Result{Status: "deadline", SessionID: sessionID, Model: r.modelNameOrDefault(), PromptVersion: "v1", Trust: "derived_untrusted", CompletedAt: now}, nil
	}
	if task == "" {
		return child.Result{}, child.Errorf(child.ErrorCodeChildInvalid, "child task must not be empty")
	}
	if r.exec == nil {
		return child.Result{}, child.Errorf(child.ErrorCodeChildUnavailable, "child executor is not configured")
	}
	if err := ctx.Err(); err != nil {
		return child.Result{}, err
	}
	projection := r.loadProjection(ctx, sessionID)
	if strings.TrimSpace(projection.ownerID) == "" {
		return child.Result{}, child.Errorf(child.ErrorCodeChildInvalid, "child owner is not resolved")
	}
	if err := r.enforceApprovalBinding(ctx, sessionID, deadline, projection); err != nil {
		return child.Result{}, err
	}
	turnReq := &runtime.TurnRequest{
		TurnID:         strings.TrimSpace(req.ChildID),
		SessionID:      sessionID,
		PrincipalID:    projection.ownerID,
		Origin:         runtime.OriginInternal,
		Parts:          []runtimeingress.InputPart{{Text: task}},
		IdempotencyKey: strings.TrimSpace(req.ChildID),
		Deadline:       deadline,
		Budget:         runtime.Budget{MaxTokens: projection.maxTokens},
	}
	if strings.TrimSpace(turnReq.TurnID) == "" {
		turnReq.TurnID = sessionID
		turnReq.IdempotencyKey = sessionID
	}
	var texts []string
	var tokens int64
	var steps int
	for ev, err := range r.exec.Execute(ctx, turnReq) {
		if err != nil {
			return child.Result{}, err
		}
		steps++
		for _, text := range childTextParts(ev.Payload) {
			if strings.TrimSpace(text) == task {
				continue
			}
			texts = append(texts, text)
		}
		tokens += childUsageTokens(ev.ProviderUsage)
	}
	output := strings.TrimSpace(strings.Join(texts, "\n"))
	if output == "" {
		return child.Result{}, child.Errorf(child.ErrorCodeChildInvalid, "child produced no output")
	}
	if len(output) > 8192 {
		output = output[:8192]
	}
	if steps == 0 {
		return child.Result{}, child.Errorf(child.ErrorCodeChildInvalid, "child produced no events")
	}
	cost := tokens * 7
	if tokens == 0 {
		tokens = childTokensFor(output)
		cost = childCostFor(output)
	}
	if projection.hasBudget {
		if projection.maxTokens > 0 && tokens > projection.maxTokens {
			return child.Result{}, child.Errorf(child.ErrorCodeBudgetExceeded, "child budget is exceeded")
		}
		if projection.maxCost > 0 && cost > projection.maxCost {
			return child.Result{}, child.Errorf(child.ErrorCodeBudgetExceeded, "child budget is exceeded")
		}
	}
	result := child.Result{
		Status: "completed", Output: output, SessionID: sessionID,
		Model: r.modelNameOrDefault(), PromptVersion: "v1", Trust: "derived_untrusted",
		TokensUsed: tokens, CostMicros: cost, CompletedAt: time.Now().UTC(),
	}
	projection.applyTo(&result)
	return result, nil
}

func (r childSessionRunner) modelNameOrDefault() string {
	if strings.TrimSpace(r.modelName) != "" {
		return strings.TrimSpace(r.modelName)
	}
	return "child-default"
}

func childTextParts(payload []byte) []string {
	if len(payload) == 0 {
		return nil
	}
	var decoded struct {
		Content *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil
	}
	if decoded.Content == nil {
		return nil
	}
	texts := make([]string, 0, len(decoded.Content.Parts))
	for _, part := range decoded.Content.Parts {
		if strings.TrimSpace(part.Text) == "" {
			continue
		}
		texts = append(texts, part.Text)
	}
	return texts
}

func childUsageTokens(usageJSON json.RawMessage) int64 {
	if len(usageJSON) == 0 {
		return 0
	}
	var usage struct {
		TotalTokenCount int32 `json:"totalTokenCount"`
	}
	if err := json.Unmarshal(usageJSON, &usage); err != nil {
		return 0
	}
	if usage.TotalTokenCount < 0 {
		return 0
	}
	return int64(usage.TotalTokenCount)
}

type childProjection struct {
	childID     string
	digest      string
	durableKey  string
	sourceRange string
	ownerID     string
	grants      []child.Grant
	hasBudget   bool
	maxTokens   int64
	maxCost     int64
}

func (r childSessionRunner) parentOwner(ctx context.Context, parentSessionID string) (string, error) {
	if r.db == nil || strings.TrimSpace(parentSessionID) == "" {
		return "", errors.New("cli: parent session must not be empty")
	}
	sessions := store.NewSessionService(r.db)
	sess, err := sessions.Get(ctx, strings.TrimSpace(parentSessionID))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(sess.OwnerID) == "" {
		return "", errors.New("cli: parent owner must not be empty")
	}
	return sess.OwnerID, nil
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
	var id, parentSession, parentInvocation, durableKey, digest, grantsJSON, budgetJSON string
	err := r.db.QueryRowContext(ctx, `SELECT id, parent_session_id, parent_invocation_id, durable_key, context_digest, grants_json, budget_json FROM child_run WHERE child_session_id = ? LIMIT 1`, sessionID).Scan(&id, &parentSession, &parentInvocation, &durableKey, &digest, &grantsJSON, &budgetJSON)
	if err != nil {
		return projection
	}
	projection.childID = id
	projection.digest = digest
	projection.durableKey = durableKey
	projection.sourceRange = parentInvocation
	if owner, ownerErr := r.parentOwner(ctx, parentSession); ownerErr == nil {
		projection.ownerID = owner
	}
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

func childTokensFor(summary string) int64 {
	return int64(len(summary)+3)/4 + 1
}

func childCostFor(summary string) int64 {
	return childTokensFor(summary) * 7
}

type allowChildGate struct{}

func (allowChildGate) Authorize(_ context.Context, _ *child.ToolRequest, _ *child.GrantBinding, _ time.Time) error {
	return nil
}

func (r childSessionRunner) enforceApprovalBinding(ctx context.Context, sessionID string, deadline time.Time, projection *childProjection) error {
	if projection == nil || strings.TrimSpace(projection.childID) == "" || strings.TrimSpace(projection.ownerID) == "" {
		return nil
	}
	spawn := &child.Spawn{
		ID: sessionID, SessionID: sessionID, OwnerID: projection.ownerID,
		Grants: projection.grants, DurableKey: projection.durableKey,
		ParentInvocation: projection.sourceRange, Deadline: deadline,
	}
	spawn.ID = projection.childID
	check := child.CheckChildToolCall(spawn, allowChildGate{}, nil)
	names := projection.grantNames()
	if len(names) == 0 {
		return nil
	}
	probe := &child.ToolRequest{
		RequestID: "probe-" + projection.childID, TurnID: "turn-" + projection.childID,
		SessionID: sessionID, PrincipalID: projection.ownerID,
		ToolName: names[0], ToolVersion: "v1",
		Deadline: deadline, IdempotencyKey: "probe-key-" + projection.childID,
	}
	if err := check(ctx, probe); err != nil {
		return err
	}
	nested := &child.ToolRequest{
		RequestID: "probe-spawn-" + projection.childID, TurnID: "turn-" + projection.childID,
		SessionID: sessionID, PrincipalID: projection.ownerID,
		ToolName: "spawn_child", ToolVersion: "v1",
		Deadline: deadline, IdempotencyKey: "probe-spawn-key-" + projection.childID,
	}
	if err := check(ctx, nested); err == nil {
		return child.Errorf(child.ErrorCodeChildSpawnDenied, "child catalogs cannot carry spawn_child")
	}
	return nil
}

func (childSessionRunner) CancelSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("cli: child session must not be empty")
	}
	return ctx.Err()
}

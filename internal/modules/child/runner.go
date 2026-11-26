package child

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

type SessionStore interface {
	Get(ctx context.Context, id string) (turn.Session, error)
	Create(ctx context.Context, session *turn.Session) error
}

type TurnRunner interface {
	Execute(ctx context.Context, req *turn.TurnRequest) iter.Seq2[turn.Event, error]
}

type StoreRegistry struct {
	store    Store
	sessions SessionStore
}

func NewStoreRegistry(store Store, sessions SessionStore) (*StoreRegistry, error) {
	if store == nil {
		return nil, errNilArgument("store")
	}
	if sessions == nil {
		return nil, errNilArgument("sessions")
	}
	return &StoreRegistry{store: store, sessions: sessions}, nil
}

func (r *StoreRegistry) Spawn(ctx context.Context, spec *Spec, now time.Time) (Spawn, bool, error) {
	if r == nil || r.store == nil {
		return Spawn{}, false, errNilArgument("store")
	}
	if r.sessions == nil {
		return Spawn{}, false, errNilArgument("sessions")
	}
	if spec == nil {
		return Spawn{}, false, errNilArgument("spec")
	}
	grants, err := json.Marshal(spec.RequestedGrants)
	if err != nil {
		return Spawn{}, false, fmt.Errorf("child: encode grants: %w", err)
	}
	budget, err := json.Marshal(spec.Budget)
	if err != nil {
		return Spawn{}, false, fmt.Errorf("child: encode budget: %w", err)
	}
	existing, found, err := r.store.GetRunByInvocation(ctx, spec.ParentInvocation, spec.IdempotencyKey)
	if err != nil {
		return Spawn{}, false, err
	}
	if found {
		if existing.ContextDigest != spec.ContextDigest {
			return Spawn{}, false, fmt.Errorf("child: invocation conflicts with an existing child: %w", conflictSentinel())
		}
		return spawnFromRun(&existing), false, nil
	}
	deadline := now.Add(spec.Budget.Timeout)
	parentDepth, err := r.Depth(ctx, spec.ParentSessionID)
	if err != nil {
		return Spawn{}, false, err
	}
	if parentDepth+1 > 1 {
		return Spawn{}, false, Errorf(ErrorCodeChildDepthExceeded, "child depth exceeds the maximum of one")
	}
	run := &Run{
		ID: spec.ID, IdempotencyKey: spec.IdempotencyKey,
		ParentSessionID: spec.ParentSessionID, ParentTurnID: spec.ParentTurnID,
		ParentInvocation: spec.ParentInvocation, ChildSessionID: spec.ChildSessionID,
		DurableKey: DurableChildKey(spec.ID), ContextDigest: spec.ContextDigest,
		GrantsJSON: string(grants), BudgetJSON: string(budget),
		State: StatusQueued, Deadline: deadline, CreatedAt: now, UpdatedAt: now,
	}
	parent, err := r.sessions.Get(ctx, spec.ParentSessionID)
	if err != nil {
		return Spawn{}, false, err
	}
	if err := r.sessions.Create(ctx, &turn.Session{
		ID: spec.ChildSessionID, OwnerID: spec.OwnerID, AgentID: parent.AgentID,
		CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`),
	}); err != nil {
		if !fault.HasCode(err, turnstore.ErrorCodeSessionIDConflict) {
			return Spawn{}, false, err
		}
	}
	if err := r.store.InsertRun(ctx, run); err != nil {
		if fault.HasCode(err, ErrorCodeChildConflict) {
			existing, found, readErr := r.store.GetRunByInvocation(ctx, spec.ParentInvocation, spec.IdempotencyKey)
			if readErr != nil {
				return Spawn{}, false, readErr
			}
			if found && existing.ContextDigest == spec.ContextDigest {
				return spawnFromRun(&existing), false, nil
			}
			return Spawn{}, false, fmt.Errorf("child: run conflicts under concurrency: %w", conflictSentinel())
		}
		return Spawn{}, false, err
	}
	return Spawn{
		ID: run.ID, SessionID: run.ChildSessionID, Grants: spec.RequestedGrants, DurableKey: run.DurableKey,
		ContextDigest: run.ContextDigest, Deadline: deadline, CreatedAt: now,
	}, true, nil
}

func (r *StoreRegistry) Get(ctx context.Context, id string) (Spawn, bool, error) {
	if r == nil || r.store == nil {
		return Spawn{}, false, errNilArgument("store")
	}
	run, found, err := r.store.GetRun(ctx, id)
	if err != nil || !found {
		return Spawn{}, found, err
	}
	return spawnFromRun(&run), true, nil
}

func (r *StoreRegistry) Depth(ctx context.Context, sessionID string) (int, error) {
	if r == nil || r.store == nil {
		return 0, errNilArgument("store")
	}
	return r.store.Depth(ctx, sessionID)
}

func (r *StoreRegistry) RunState(ctx context.Context, id string) (string, error) {
	if r == nil || r.store == nil {
		return "", errNilArgument("store")
	}
	run, found, err := r.store.GetRun(ctx, id)
	if err != nil {
		return "", err
	}
	if !found {
		return "", Errorf(ErrorCodeChildNotFound, "child is not found")
	}
	return run.State, nil
}

func (r *StoreRegistry) SetChildState(ctx context.Context, id, state string, now time.Time) error {
	if r == nil || r.store == nil {
		return errNilArgument("store")
	}
	return r.store.SetState(ctx, id, state, now)
}

func (r *StoreRegistry) ActiveForParent(ctx context.Context, parentSessionID string) ([]Spawn, error) {
	if r == nil || r.store == nil {
		return nil, errNilArgument("store")
	}
	runs, err := r.store.ActiveForParent(ctx, parentSessionID)
	if err != nil {
		return nil, err
	}
	spawns := make([]Spawn, 0, len(runs))
	for i := range runs {
		spawns = append(spawns, spawnFromRun(&runs[i]))
	}
	return spawns, nil
}

func spawnFromRun(run *Run) Spawn {
	var grants []Grant
	if err := json.Unmarshal([]byte(run.GrantsJSON), &grants); err != nil {
		grants = nil
	}
	return Spawn{
		ID: run.ID, SessionID: run.ChildSessionID, Grants: grants, DurableKey: run.DurableKey,
		ContextDigest: run.ContextDigest, Deadline: run.Deadline, CreatedAt: run.CreatedAt,
	}
}

func conflictSentinel() error {
	return Errorf(ErrorCodeChildConflict, "child run conflicts")
}

type StoreHandlerRuns struct {
	store Store
}

func NewStoreHandlerRuns(store Store) (*StoreHandlerRuns, error) {
	if store == nil {
		return nil, errNilArgument("store")
	}
	return &StoreHandlerRuns{store: store}, nil
}

func (r *StoreHandlerRuns) GetRun(ctx context.Context, id string) (HandlerRun, bool, error) {
	if r == nil || r.store == nil {
		return HandlerRun{}, false, errNilArgument("store")
	}
	run, found, err := r.store.GetRun(ctx, id)
	if err != nil || !found {
		return HandlerRun{}, found, err
	}
	return HandlerRun{
		ID: run.ID, SessionID: run.ChildSessionID, State: run.State,
		Deadline: run.Deadline, GrantsJSON: run.GrantsJSON, ContextDigest: run.ContextDigest,
		DurableKey: run.DurableKey, ParentInvocation: run.ParentInvocation,
	}, true, nil
}

func (r *StoreHandlerRuns) SetState(ctx context.Context, id, state string, now time.Time) error {
	if r == nil || r.store == nil {
		return errNilArgument("store")
	}
	return r.store.SetState(ctx, id, state, now)
}

func (r *StoreHandlerRuns) SetResult(ctx context.Context, id string, result *Result, now time.Time) error {
	if r == nil || r.store == nil {
		return errNilArgument("store")
	}
	if result == nil {
		return errNilArgument("result")
	}
	run, found, err := r.store.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return Errorf(ErrorCodeChildNotFound, "child is not found")
	}
	artifactsJSON := "[]"
	if len(result.Artifacts) > 0 {
		encoded, err := json.Marshal(result.Artifacts)
		if err != nil {
			return fmt.Errorf("child: encode result artifacts: %w", err)
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
	return r.store.SetResult(ctx, id, &StoredResult{
		Status: result.Status, Output: result.Output, ArtifactsJSON: artifactsJSON,
		TokensUsed: result.TokensUsed, CostMicros: result.CostMicros,
		CompletedAt: completedAt, Provenance: provenance,
		ChildID: childID, SessionID: sessionID, ContextDigest: digest, DurableKey: durableKey,
		SourceRange: sourceRange, Model: model, PromptVersion: promptVersion, Trust: trust,
	}, now)
}

type DurableCanceller struct {
	runtime durable.Runtime
}

func NewDurableCanceller(runtime durable.Runtime) (*DurableCanceller, error) {
	if runtime == nil {
		return nil, errNilArgument("runtime")
	}
	return &DurableCanceller{runtime: runtime}, nil
}

func (d *DurableCanceller) CancelRun(ctx context.Context, durableKey string) error {
	if d == nil || d.runtime == nil {
		return errNilArgument("runtime")
	}
	if durableKey == "" {
		return Errorf(ErrorCodeInvalidArgument, "durable key must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_ = d.runtime.Signal(ctx, durable.RunRef{Key: durableKey}, "child-cancel/"+durableKey, []byte("cancel"))
	return d.runtime.Cancel(ctx, durable.RunRef{Key: durableKey})
}

type RuntimeSignaler struct {
	runtime durable.Runtime
}

func NewRuntimeSignaler(runtime durable.Runtime) (*RuntimeSignaler, error) {
	if runtime == nil {
		return nil, errNilArgument("runtime")
	}
	return &RuntimeSignaler{runtime: runtime}, nil
}

func (s *RuntimeSignaler) SignalChild(ctx context.Context, durableKey, signal string, payload []byte) error {
	if s == nil || s.runtime == nil {
		return errNilArgument("runtime")
	}
	if strings.TrimSpace(durableKey) == "" || strings.TrimSpace(signal) == "" {
		return Errorf(ErrorCodeInvalidArgument, "durable key and signal must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.runtime.Signal(ctx, durable.RunRef{Key: strings.TrimSpace(durableKey)}, strings.TrimSpace(signal), payload)
}

type SessionRunnerDeps struct {
	Store     Store
	Sessions  SessionStore
	Turns     TurnRunner
	ModelName string
}

type SessionRunner struct {
	store     Store
	sessions  SessionStore
	turns     TurnRunner
	modelName string
}

func NewSessionRunner(deps *SessionRunnerDeps) (*SessionRunner, error) {
	if deps == nil {
		return nil, errNilArgument("deps")
	}
	return &SessionRunner{
		store:     deps.Store,
		sessions:  deps.Sessions,
		turns:     deps.Turns,
		modelName: deps.ModelName,
	}, nil
}

func (r *SessionRunner) RunSession(ctx context.Context, req RunRequest) (Result, error) {
	if r == nil {
		return Result{}, errNilArgument("runner")
	}
	sessionID := strings.TrimSpace(req.SessionID)
	deadline := req.Deadline
	task := strings.TrimSpace(req.Task)
	if sessionID == "" {
		return Result{}, Errorf(ErrorCodeInvalidArgument, "child session must not be empty")
	}
	now := time.Now().UTC()
	if !deadline.IsZero() && !now.Before(deadline.UTC()) {
		return Result{Status: "deadline", SessionID: sessionID, Model: r.modelNameOrDefault(), PromptVersion: "v1", Trust: "derived_untrusted", CompletedAt: now}, nil
	}
	if task == "" {
		return Result{}, Errorf(ErrorCodeChildInvalid, "child task must not be empty")
	}
	if r.turns == nil {
		return Result{}, Errorf(ErrorCodeChildUnavailable, "child executor is not configured")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	projection := r.loadProjection(ctx, sessionID)
	if strings.TrimSpace(projection.ownerID) == "" {
		return Result{}, Errorf(ErrorCodeChildInvalid, "child owner is not resolved")
	}
	if err := r.enforceApprovalBinding(ctx, sessionID, deadline, projection); err != nil {
		return Result{}, err
	}
	turnReq := childTurnRequest(sessionID, strings.TrimSpace(req.ChildID), task, deadline, projection)
	var texts []string
	var tokens int64
	var steps int
	for ev, err := range r.turns.Execute(ctx, turnReq) {
		if err != nil {
			return Result{}, err
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
		return Result{}, Errorf(ErrorCodeChildInvalid, "child produced no output")
	}
	if len(output) > 8192 {
		output = output[:8192]
	}
	if steps == 0 {
		return Result{}, Errorf(ErrorCodeChildInvalid, "child produced no events")
	}
	cost := tokens * 7
	if tokens == 0 {
		tokens = childTokensFor(output)
		cost = childCostFor(output)
	}
	if projection.hasBudget {
		if projection.maxTokens > 0 && tokens > projection.maxTokens {
			return Result{}, Errorf(ErrorCodeBudgetExceeded, "child budget is exceeded")
		}
		if projection.maxCost > 0 && cost > projection.maxCost {
			return Result{}, Errorf(ErrorCodeBudgetExceeded, "child budget is exceeded")
		}
	}
	result := Result{
		Status: "completed", Output: output, SessionID: sessionID,
		Model: r.modelNameOrDefault(), PromptVersion: "v1", Trust: "derived_untrusted",
		TokensUsed: tokens, CostMicros: cost, CompletedAt: time.Now().UTC(),
	}
	projection.applyTo(&result)
	return result, nil
}

func (r *SessionRunner) CancelSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return Errorf(ErrorCodeInvalidArgument, "child session must not be empty")
	}
	return ctx.Err()
}

func (r *SessionRunner) modelNameOrDefault() string {
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
	agentID     string
	grants      []Grant
	hasBudget   bool
	maxTokens   int64
	maxCost     int64
}

func childTurnRequest(sessionID, childID, task string, deadline time.Time, projection *childProjection) *turn.TurnRequest {
	turnReq := &turn.TurnRequest{
		TurnID:         childID,
		SessionID:      sessionID,
		PrincipalID:    projection.ownerID,
		PrincipalClass: principal.ClassForPrincipal(projection.ownerID),
		Origin:         turn.OriginChild,
		Parts:          []runtimeingress.InputPart{{Text: task}},
		IdempotencyKey: childID,
		Deadline:       deadline,
		Budget:         turn.Budget{MaxTokens: projection.maxTokens},
		AgentID:        projection.agentID,
	}
	if turnReq.TurnID == "" {
		turnReq.TurnID = sessionID
		turnReq.IdempotencyKey = sessionID
	}
	return turnReq
}

func (r *SessionRunner) parentSession(ctx context.Context, parentSessionID string) (turn.Session, error) {
	if r.sessions == nil || strings.TrimSpace(parentSessionID) == "" {
		return turn.Session{}, Errorf(ErrorCodeInvalidArgument, "parent session must not be empty")
	}
	sess, err := r.sessions.Get(ctx, strings.TrimSpace(parentSessionID))
	if err != nil {
		return turn.Session{}, err
	}
	if strings.TrimSpace(sess.OwnerID) == "" {
		return turn.Session{}, Errorf(ErrorCodeChildInvalid, "parent owner must not be empty")
	}
	return sess, nil
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

func (p *childProjection) applyTo(result *Result) {
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

func (r *SessionRunner) loadProjection(ctx context.Context, sessionID string) *childProjection {
	projection := &childProjection{}
	if r.store == nil {
		return projection
	}
	run, found, err := r.store.GetByChildSession(ctx, sessionID)
	if err != nil || !found {
		return projection
	}
	projection.childID = run.ID
	projection.digest = run.ContextDigest
	projection.durableKey = run.DurableKey
	projection.sourceRange = run.ParentInvocation
	if parent, parentErr := r.parentSession(ctx, run.ParentSessionID); parentErr == nil {
		projection.ownerID = parent.OwnerID
		projection.agentID = parent.AgentID
	}
	if json.Valid([]byte(run.GrantsJSON)) {
		var grants []Grant
		if err := json.Unmarshal([]byte(run.GrantsJSON), &grants); err == nil {
			valid := grants[:0]
			for _, grant := range grants {
				if strings.EqualFold(strings.TrimSpace(grant.Capability), spawnToolName) {
					continue
				}
				valid = append(valid, grant)
			}
			projection.grants = valid
		}
	}
	if json.Valid([]byte(run.BudgetJSON)) {
		var budget Budget
		if err := json.Unmarshal([]byte(run.BudgetJSON), &budget); err == nil {
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

func (allowChildGate) Authorize(_ context.Context, _ *ToolRequest, _ *GrantBinding, _ time.Time) error {
	return nil
}

func (r *SessionRunner) enforceApprovalBinding(ctx context.Context, sessionID string, deadline time.Time, projection *childProjection) error {
	if projection == nil || strings.TrimSpace(projection.childID) == "" || strings.TrimSpace(projection.ownerID) == "" {
		return nil
	}
	spawn := &Spawn{
		ID: projection.childID, SessionID: sessionID, OwnerID: projection.ownerID,
		Grants: projection.grants, DurableKey: projection.durableKey,
		ParentInvocation: projection.sourceRange, Deadline: deadline,
	}
	check := CheckChildToolCall(spawn, allowChildGate{}, nil)
	names := projection.grantNames()
	if len(names) == 0 {
		return nil
	}
	probe := &ToolRequest{
		RequestID: "probe-" + projection.childID, TurnID: "turn-" + projection.childID,
		SessionID: sessionID, PrincipalID: projection.ownerID,
		ToolName: names[0], ToolVersion: "v1",
		Deadline: deadline, IdempotencyKey: "probe-key-" + projection.childID,
	}
	if err := check(ctx, probe); err != nil {
		return err
	}
	nested := &ToolRequest{
		RequestID: "probe-spawn-" + projection.childID, TurnID: "turn-" + projection.childID,
		SessionID: sessionID, PrincipalID: projection.ownerID,
		ToolName: spawnToolName, ToolVersion: "v1",
		Deadline: deadline, IdempotencyKey: "probe-spawn-key-" + projection.childID,
	}
	if err := check(ctx, nested); err == nil {
		return Errorf(ErrorCodeChildSpawnDenied, "child catalogs cannot carry spawn_child")
	}
	return nil
}

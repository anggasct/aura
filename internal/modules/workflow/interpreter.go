package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
)

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

const HandlerName = "workflow"

type Driver int

const (
	DriverSerial Driver = iota
	DriverConcurrent
)

type Options struct {
	MaxConcurrentSteps int
	HandlerName        string
	Driver             Driver
	AgentResolver      AgentResolver
	Agents             AgentRunner
	Tools              ToolRunner
	Artifacts          ArtifactSink
	Approvals          ApprovalRequester
	Logger             logger
	GeneratedBudget    Budget
}

type logger interface {
	InfoContext(ctx context.Context, msg string, args ...any)
	WarnContext(ctx context.Context, msg string, args ...any)
}

type Interpreter struct {
	store    *Store
	durable  durable.Runtime
	specs    *definitionRegistry
	options  *Options
	inflight chan struct{}
	depsMu   sync.Mutex
	deps     ValidationDeps
	// writeMu serializes run-status writes across step goroutines; it is
	// never held while another mutex is acquired.
	writeMu sync.Mutex
}

type definitionRegistry struct {
	mu    sync.Mutex
	specs map[string]*compiledSpec
}

type compiledSpec struct {
	spec  *Spec
	graph *Graph
}

func (r *definitionRegistry) put(spec *Spec, graph *Graph) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs[spec.ID] = &compiledSpec{spec: spec, graph: graph}
}

func (r *definitionRegistry) get(id string) (*compiledSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	compiled, ok := r.specs[id]
	return compiled, ok
}

func (r *definitionRegistry) list() []*Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	specs := make([]*Spec, 0, len(r.specs))
	for _, compiled := range r.specs {
		if compiled.spec.Source != SourceGenerated || compiled.spec.Name != "" {
			specs = append(specs, compiled.spec)
		}
	}
	slices.SortFunc(specs, func(a, b *Spec) int {
		return strings.Compare(a.ID, b.ID)
	})
	return specs
}

func NewInterpreter(store *Store, runtime durable.Runtime, options *Options) *Interpreter {
	if options == nil {
		options = &Options{}
	}
	if options.MaxConcurrentSteps <= 0 {
		options.MaxConcurrentSteps = 4
	}
	if options.HandlerName == "" {
		options.HandlerName = HandlerName
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	interpreter := &Interpreter{
		store:    store,
		durable:  runtime,
		specs:    &definitionRegistry{specs: map[string]*compiledSpec{}},
		options:  options,
		inflight: make(chan struct{}, options.MaxConcurrentSteps),
	}
	if registrar, ok := runtime.(durable.HandlerRegistrar); ok {
		registrar.RegisterHandler(options.HandlerName, interpreter.handlerFor(options.Driver))
	}
	return interpreter
}

func (i *Interpreter) handlerFor(driver Driver) durable.Handler {
	if driver == DriverConcurrent {
		return i.handleInvocation
	}
	return i.HandleSerial
}

func (i *Interpreter) compiledFor(ctx context.Context, id string, version int) (*compiledSpec, bool) {
	cached, present := i.specs.get(id)
	if present && (version == 0 || cached.spec.Version == version) {
		return cached, true
	}
	var spec *Spec
	var err error
	if version == 0 {
		spec, err = i.store.LatestDefinition(ctx, id)
	} else {
		spec, err = i.store.Definition(ctx, id, version)
	}
	if err != nil {
		return nil, false
	}
	deps := i.ValidationDeps()
	graph, err := Compile(spec, deps)
	if err != nil {
		i.options.Logger.WarnContext(ctx, "stored workflow definition does not compile", "component", "workflow", "definition_id", id, "version", version, "error", err)
		return nil, false
	}
	if !present {
		i.specs.put(spec, graph)
	}
	return &compiledSpec{spec: spec, graph: graph}, true
}

func (i *Interpreter) LoadAll(ctx context.Context, specs []*Spec, deps *ValidationDeps) error {
	return i.load(ctx, specs, deps, nil)
}

type Quarantined struct {
	ID      string
	Version int
	Err     error
}

func (i *Interpreter) LoadRoutines(ctx context.Context, specs []*Spec, deps *ValidationDeps) ([]Quarantined, error) {
	i.setDeps(deps)
	var quarantined []Quarantined
	pending := make(map[string]*Spec, len(specs))
	graphs := make(map[string]*Graph, len(specs))
	for _, spec := range specs {
		if spec == nil {
			continue
		}
		graph, err := Compile(spec, deps)
		if err != nil {
			quarantined = append(quarantined, Quarantined{ID: spec.ID, Version: spec.Version, Err: err})
			continue
		}
		pending[spec.ID] = spec
		graphs[spec.ID] = graph
	}
	lookup := func(id string) (*Spec, bool) {
		if spec, ok := pending[id]; ok {
			return spec, true
		}
		return i.Spec(id)
	}
	for changed := true; changed; {
		changed = false
		for _, spec := range specs {
			if spec == nil || pending[spec.ID] == nil {
				continue
			}
			if err := ValidateComposition(spec, lookup); err != nil {
				quarantined = append(quarantined, Quarantined{ID: spec.ID, Version: spec.Version, Err: err})
				delete(pending, spec.ID)
				delete(graphs, spec.ID)
				changed = true
			}
		}
	}
	for _, spec := range specs {
		if spec == nil || pending[spec.ID] == nil {
			continue
		}
		if err := i.store.saveDefinition(ctx, spec, nil); err != nil {
			return nil, err
		}
		i.specs.put(spec, graphs[spec.ID])
	}
	return quarantined, nil
}

func (i *Interpreter) Install(ctx context.Context, specs []*Spec, deps *ValidationDeps) error {
	return i.load(ctx, specs, deps, &Approval{Kind: ApprovalInstalled, ApproverID: principal.OwnerID})
}

func (i *Interpreter) load(ctx context.Context, specs []*Spec, deps *ValidationDeps, approval *Approval) error {
	i.setDeps(deps)
	pending := make(map[string]*Spec, len(specs))
	for _, spec := range specs {
		if spec != nil {
			pending[spec.ID] = spec
		}
	}
	lookup := func(id string) (*Spec, bool) {
		if spec, ok := pending[id]; ok {
			return spec, true
		}
		return i.Spec(id)
	}
	graphs := make([]*Graph, len(specs))
	for index, spec := range specs {
		graph, err := Compile(spec, deps)
		if err != nil {
			return fmt.Errorf("definition %s: %w", spec.ID, err)
		}
		if err := ValidateComposition(spec, lookup); err != nil {
			return fmt.Errorf("definition %s: %w", spec.ID, err)
		}
		graphs[index] = graph
	}
	for index, spec := range specs {
		if err := i.store.saveDefinition(ctx, spec, approval); err != nil {
			return err
		}
		i.specs.put(spec, graphs[index])
	}
	return nil
}

func (i *Interpreter) Definitions() []*Spec {
	return i.specs.list()
}

func (i *Interpreter) Spec(id string) (*Spec, bool) {
	compiled, ok := i.specs.get(id)
	if !ok {
		return nil, false
	}
	return compiled.spec, true
}

func (i *Interpreter) Start(ctx context.Context, definitionID string, input *RunInput, origin *RunOrigin) (*RunSummary, error) {
	return i.start(ctx, &startRequest{definitionID: definitionID, input: input, origin: origin, durableKey: newID("dur")})
}

func (i *Interpreter) StartApproved(ctx context.Context, proposalID string, input *RunInput, origin *RunOrigin) (*RunSummary, error) {
	approval, err := i.store.proposalApproval(ctx, proposalID)
	if err != nil {
		return nil, err
	}
	return i.start(ctx, &startRequest{definitionID: approval.DefinitionID, input: input, origin: origin, durableKey: newID("dur"), claimProposalID: proposalID})
}

func (i *Interpreter) StartConfigured(ctx context.Context, definitionID string, input *RunInput, principalID string, class principal.Class, durableKey string) (*RunSummary, error) {
	if durableKey == "" {
		durableKey = newID("dur")
	} else {
		existing, found, err := i.store.RunByDurableKey(ctx, durableKey)
		if err != nil {
			return nil, err
		}
		if found {
			return existing, nil
		}
	}
	origin := &RunOrigin{PrincipalID: principalID, PrincipalClass: string(class)}
	return i.start(ctx, &startRequest{definitionID: definitionID, input: input, origin: origin, durableKey: durableKey, configured: true})
}

type startRequest struct {
	definitionID    string
	input           *RunInput
	origin          *RunOrigin
	durableKey      string
	claimProposalID string
	configured      bool
}

func (i *Interpreter) start(ctx context.Context, req *startRequest) (*RunSummary, error) {
	compiled, ok := i.compiledFor(ctx, req.definitionID, 0)
	if !ok {
		return nil, codedError(ErrorCodeDefinitionNotFound, "definition "+req.definitionID+" is not loaded")
	}
	approval, err := i.requireSpecApproval(ctx, compiled.spec)
	if err != nil {
		return nil, err
	}
	claim, err := proposalClaimFor(approval, req.claimProposalID)
	if err != nil {
		return nil, err
	}
	runOrigin, err := originUnderApproval(req.origin, approval)
	if err != nil {
		return nil, err
	}
	if req.configured {
		runOrigin.AgentID = configuredAgent(compiled.spec, approval)
	}
	if err := i.admitRun(ctx, compiled.spec, runOrigin); err != nil {
		return nil, err
	}
	summary, err := i.store.createRun(ctx, &runCreate{Spec: compiled.spec, Input: req.input, Origin: runOrigin, DurableKey: req.durableKey, ClaimProposalID: claim})
	if err != nil {
		return nil, err
	}
	if _, err := i.durable.Start(ctx, durable.StartRequest{
		Handler: i.options.HandlerName,
		Key:     summary.DurableKey,
		Payload: mustEncode(tickPayload{RunID: summary.ID, DefinitionID: compiled.spec.ID, Version: compiled.spec.Version}),
	}); err != nil {
		return nil, fmt.Errorf("start durable run: %w", err)
	}
	return summary, nil
}

type tickPayload struct {
	RunID        string `json:"run_id"`
	DefinitionID string `json:"definition_id"`
	Version      int    `json:"version"`
}

func mustEncode(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("workflow: encode tick: %v", err))
	}
	return encoded
}

func (i *Interpreter) Signal(ctx context.Context, runID, name string, payload []byte) error {
	summary, err := i.store.Run(ctx, runID)
	if err != nil {
		return err
	}
	return i.durable.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, name, payload)
}

func (i *Interpreter) Cancel(ctx context.Context, runID string) error {
	summary, err := i.store.Run(ctx, runID)
	if err != nil {
		return err
	}
	return i.durable.Cancel(ctx, durable.RunRef{Key: summary.DurableKey})
}

func (i *Interpreter) RunStatus(ctx context.Context, runID string) (string, error) {
	summary, err := i.store.Run(ctx, runID)
	if err != nil {
		return "", err
	}
	return summary.Status, nil
}

func (i *Interpreter) handleInvocation(ctx context.Context, inv durable.Invocation) error {
	var tick tickPayload
	if err := json.Unmarshal(inv.Payload(), &tick); err != nil {
		return fmt.Errorf("decode invocation payload: %w", err)
	}
	compiled, ok := i.compiledFor(ctx, tick.DefinitionID, tick.Version)
	if !ok {
		return codedError(ErrorCodeDefinitionNotFound, "definition "+tick.DefinitionID+" is not loaded")
	}
	summary, err := i.store.Run(ctx, tick.RunID)
	if err != nil {
		return err
	}
	digest, err := Digest(compiled.spec)
	if err != nil {
		return err
	}
	input, err := i.store.RunInputFor(ctx, tick.RunID)
	if err != nil {
		return err
	}
	if err := i.store.SetRunStatus(ctx, tick.RunID, RunRunning); err != nil {
		return err
	}
	execution := &stepExecution{
		interpreter:      i,
		invocation:       inv,
		spec:             compiled.spec,
		graph:            compiled.graph,
		runID:            tick.RunID,
		origin:           summary.Origin,
		definitionDigest: digest,
		input:            input,
		awaiting:         map[string]bool{},
		budget:           i.budgetFor(compiled.spec, nil),
	}
	return execution.run(ctx)
}

type stepExecution struct {
	interpreter      *Interpreter
	invocation       durable.Invocation
	spec             *Spec
	graph            *Graph
	definitionDigest string
	runID            string
	origin           RunOrigin
	input            *RunInput
	outputs          map[string]json.RawMessage
	statuses         map[string]string
	failedStep       string
	budget           *runBudget
	mu               sync.Mutex
	// statusMu guards the in-memory awaiting set of steps currently
	// suspended on a signal; it is never held across a database write.
	statusMu sync.Mutex
	awaiting map[string]bool
}

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

	"github.com/anggasct/aura/internal/durable"
)

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// Options configures the interpreter.
type Options struct {
	// MaxConcurrentSteps bounds concurrently running steps; overflow waits.
	MaxConcurrentSteps int
	// HandlerName is the durable service name the interpreter registers.
	HandlerName string
	// AgentResolver resolves agent steps (the agent registry).
	AgentResolver AgentResolver
	// Agents runs bounded agent executions.
	Agents AgentRunner
	// Tools invokes tools through the broker.
	Tools ToolRunner
	// Artifacts stores oversized step outputs content-addressed.
	Artifacts ArtifactSink
	// Approvals binds approval requests to the run+step.
	Approvals ApprovalRequester
	Logger    logger
}

type logger interface {
	InfoContext(ctx context.Context, msg string, args ...any)
	WarnContext(ctx context.Context, msg string, args ...any)
}

// Interpreter executes compiled workflow specs through the DurableRuntime
// port, persisting run and step transitions transactionally to the
// projections.
type Interpreter struct {
	store    *Store
	durable  durable.Runtime
	specs    *definitionRegistry
	options  *Options
	inflight chan struct{}
	// writeMu serializes run-status writes across step goroutines; it is
	// never held while another mutex is acquired.
	writeMu sync.Mutex
}

type definitionRegistry struct {
	mu    sync.Mutex
	specs map[string]*compiledSpec
}

// compiledSpec pairs a registered spec with its validated execution graph;
// the graph is built once, at load.
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
		specs = append(specs, compiled.spec)
	}
	slices.SortFunc(specs, func(a, b *Spec) int {
		return strings.Compare(a.ID, b.ID)
	})
	return specs
}

// NewInterpreter wires the single workflow engine over the port.
func NewInterpreter(store *Store, runtime durable.Runtime, options *Options) *Interpreter {
	if options == nil {
		options = &Options{}
	}
	if options.MaxConcurrentSteps <= 0 {
		options.MaxConcurrentSteps = 4
	}
	if options.HandlerName == "" {
		options.HandlerName = "workflow"
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
		registrar.RegisterHandler(options.HandlerName, interpreter.handleInvocation)
	}
	return interpreter
}

// Load validates and registers one spec version; an invalid spec never
// executes or persists a run.
func (i *Interpreter) Load(ctx context.Context, spec *Spec, deps ValidationDeps) error {
	graph, err := Compile(spec, deps)
	if err != nil {
		return err
	}
	if err := i.store.SaveDefinition(ctx, spec); err != nil {
		return err
	}
	i.specs.put(spec, graph)
	return nil
}

// Definitions lists registered specs.
func (i *Interpreter) Definitions() []*Spec {
	return i.specs.list()
}

// Spec returns one registered spec.
func (i *Interpreter) Spec(id string) (*Spec, bool) {
	compiled, ok := i.specs.get(id)
	if !ok {
		return nil, false
	}
	return compiled.spec, true
}

// Start validates, persists, and enqueues a run through the durable port.
func (i *Interpreter) Start(ctx context.Context, definitionID string, input *RunInput) (*RunSummary, error) {
	compiled, ok := i.specs.get(definitionID)
	if !ok {
		return nil, codedError(ErrorCodeDefinitionNotFound, "definition "+definitionID+" is not loaded")
	}
	summary, err := i.store.CreateRun(ctx, compiled.spec, input)
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

// Signal forwards a runtime signal (wait or approval resolution).
func (i *Interpreter) Signal(ctx context.Context, runID, name string, payload []byte) error {
	summary, err := i.store.Run(ctx, runID)
	if err != nil {
		return err
	}
	return i.durable.Signal(ctx, durable.RunRef{Key: summary.DurableKey}, name, payload)
}

// Cancel cooperatively cancels the durable run.
func (i *Interpreter) Cancel(ctx context.Context, runID string) error {
	summary, err := i.store.Run(ctx, runID)
	if err != nil {
		return err
	}
	return i.durable.Cancel(ctx, durable.RunRef{Key: summary.DurableKey})
}

// RunStatus maps the durable state onto the projection.
func (i *Interpreter) RunStatus(ctx context.Context, runID string) (string, error) {
	summary, err := i.store.Run(ctx, runID)
	if err != nil {
		return "", err
	}
	return summary.Status, nil
}

// handleInvocation drives one run: dependency-ready steps execute
// concurrently up to the bound; wait and approval steps suspend on their
// signals; terminal states persist transactionally.
func (i *Interpreter) handleInvocation(ctx context.Context, inv *durable.Invocation) error {
	var tick tickPayload
	if err := json.Unmarshal(inv.Payload(), &tick); err != nil {
		return fmt.Errorf("decode invocation payload: %w", err)
	}
	compiled, ok := i.specs.get(tick.DefinitionID)
	if !ok {
		return codedError(ErrorCodeDefinitionNotFound, "definition "+tick.DefinitionID+" is not loaded")
	}
	input, err := i.store.RunInputFor(ctx, tick.RunID)
	if err != nil {
		return err
	}
	if err := i.store.SetRunStatus(ctx, tick.RunID, RunRunning); err != nil {
		return err
	}
	execution := &stepExecution{
		interpreter: i,
		invocation:  inv,
		spec:        compiled.spec,
		graph:       compiled.graph,
		runID:       tick.RunID,
		input:       input,
		awaiting:    map[string]bool{},
	}
	return execution.run(ctx)
}

// stepExecution carries one in-flight run's working state.
type stepExecution struct {
	interpreter *Interpreter
	invocation  *durable.Invocation
	spec        *Spec
	graph       *Graph
	runID       string
	input       *RunInput
	// outputs and statuses are handler-local working state; the durable
	// journal replays them from the same deterministic order.
	outputs    map[string]json.RawMessage
	statuses   map[string]string
	failedStep string
	// mu guards the working state shared across concurrent step goroutines.
	mu sync.Mutex
	// statusMu guards the in-memory awaiting set of steps currently
	// suspended on a signal; it is never held across a database write.
	statusMu sync.Mutex
	awaiting map[string]bool
}

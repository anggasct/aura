package schedule

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
)

type OwnerDestination interface {
	OwnerRoute() (alias, route string)
}

type WorkflowChecker func(ctx context.Context, id string) error

type OperationDeps struct {
	Config          Config
	Store           *SQLiteStore
	Service         *Service
	Starter         Starter
	WorkflowChecker WorkflowChecker
	Owner           OwnerDestination
	Now             func() time.Time
}

type Operations struct {
	config   Config
	store    *SQLiteStore
	service  *Service
	starter  Starter
	workflow WorkflowChecker
	owner    OwnerDestination
	now      func() time.Time
}

func NewOperations(deps *OperationDeps) (*Operations, error) {
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "schedule operation deps must not be nil")
	}
	if deps.Store == nil || deps.Service == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "schedule operations need the job store and service")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Operations{
		config: deps.Config, store: deps.Store, service: deps.Service, starter: deps.Starter,
		workflow: deps.WorkflowChecker, owner: deps.Owner, now: now,
	}, nil
}

func (o *Operations) Bindings() []operation.Binding {
	return []operation.Binding{
		{ID: operationAdd, Handle: o.add, Preview: o.previewAdd},
		{ID: operationDel, Handle: o.setState(JobDeleted)},
		{ID: operationList, Handle: o.list},
		{ID: operationPause, Handle: o.setState(JobPaused)},
		{ID: operationResum, Handle: o.resume},
		{ID: operationNow, Handle: o.runNow},
		{ID: operationConf, Handle: o.confirm, Preview: o.previewConfirm},
		{ID: operationRuns, Handle: o.runs},
	}
}

type addInput struct {
	Name        string          `json:"name"`
	Schedule    string          `json:"schedule"`
	Timezone    string          `json:"timezone"`
	Channel     string          `json:"channel"`
	Destination string          `json:"destination"`
	Prompt      string          `json:"prompt"`
	Workflow    string          `json:"workflow"`
	Context     string          `json:"context"`
	Input       json.RawMessage `json:"input"`
	Overlap     string          `json:"overlap"`
	Grace       string          `json:"grace"`
}

type jobInput struct {
	ID      string `json:"id"`
	Limit   int    `json:"limit"`
	Context string `json:"context"`
}

func decodeInput(call *operation.Call, into any) error {
	if err := json.Unmarshal(call.Input, into); err != nil {
		return operation.NewError(operation.ErrorCodeInputInvalid, "the input cannot be read")
	}
	return nil
}

func usage(detail string) error {
	return operation.NewError(operation.ErrorCodeInputInvalid, detail)
}

func (o *Operations) jobID(call *operation.Call) (string, error) {
	var in jobInput
	if err := decodeInput(call, &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.ID) == "" {
		return "", usage("the job id must not be blank")
	}
	return in.ID, nil
}

func (o *Operations) ownerTarget() (alias, route string, err error) {
	if o.owner != nil {
		alias, route = o.owner.OwnerRoute()
	}
	if alias == "" || route == "" {
		return "", "", operation.NotPerformed(ErrorCodeCapabilityUnavailable,
			fault.New(ErrorCodeCapabilityUnavailable, "no owner destination is configured"))
	}
	return alias, route, nil
}

func (o *Operations) spec(call *operation.Call) (*JobSpec, error) {
	var in addInput
	if err := decodeInput(call, &in); err != nil {
		return nil, err
	}
	if (in.Prompt == "") == (in.Workflow == "") {
		return nil, usage("exactly one of prompt or workflow is required")
	}
	if len(in.Input) > 0 && in.Workflow == "" {
		return nil, usage("input requires workflow")
	}
	if in.Prompt != "" && in.Context == "" {
		return nil, usage("context is required with a prompt: full or web_only")
	}
	if in.Workflow != "" && in.Context != "" {
		return nil, usage("context applies to a prompt, not to a workflow")
	}
	if err := validateWorkflowInput(in.Input); err != nil {
		return nil, err
	}
	channel, destination := in.Channel, in.Destination
	if call.Caller.Surface == operation.SurfaceAgent {
		alias, route, err := o.ownerTarget()
		if err != nil {
			return nil, err
		}
		channel, _, _ = strings.Cut(route, ":")
		destination = alias
	}
	graceSeconds := int64(time.Duration(o.config.DefaultCatchUpGrace).Seconds())
	if in.Grace != "" {
		grace, err := time.ParseDuration(in.Grace)
		if err != nil {
			return nil, usage("grace is not a duration")
		}
		graceSeconds = int64(grace.Seconds())
	}
	spec := &JobSpec{
		Name: in.Name, CronExpression: in.Schedule, Timezone: cmp.Or(in.Timezone, o.config.DefaultTimezone),
		Prompt: in.Prompt, OriginChannel: channel, OriginDestination: destination,
		OverlapPolicy: in.Overlap, CatchUpGraceSeconds: graceSeconds, WorkflowID: in.Workflow,
		WorkflowInput: string(in.Input), Context: in.Context, TaintSource: callerTaintSource(&call.Caller),
	}
	if err := validateJobSpec(spec); err != nil {
		return nil, invalidSpec(err)
	}
	return spec, nil
}

func callerTaintSource(caller *operation.Caller) string {
	if !caller.Tainted && caller.TaintSource == "" {
		return ""
	}
	return cmp.Or(caller.TaintSource, unknownTaintLabel)
}

func invalidSpec(err error) error {
	return operation.NewError(operation.ErrorCodeInputInvalid, err.Error())
}

func (o *Operations) checkWorkflow(ctx context.Context, spec *JobSpec) error {
	if spec.WorkflowID == "" {
		return nil
	}
	if o.workflow == nil {
		return fault.New(ErrorCodeInvalidArgument, "workflow checker must not be nil")
	}
	return o.workflow(ctx, spec.WorkflowID)
}

func (o *Operations) previewAdd(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	spec, err := o.spec(call)
	if err != nil {
		return operation.Preview{}, err
	}
	if err := o.checkWorkflow(ctx, spec); err != nil {
		return operation.Preview{}, err
	}
	return previewOf(spec, o.routeOf(spec.OriginDestination)), nil
}

func (o *Operations) routeOf(destination string) string {
	if o.owner == nil {
		return ""
	}
	if alias, route := o.owner.OwnerRoute(); alias == destination {
		return route
	}
	return ""
}

const contextNone = "-"

const (
	contextFullLine    = "Context: full (the persona's memory, profile, knowledge and skills, and its tools)"
	contextWebOnlyLine = "Context: web only (no memory, profile, knowledge or skills; tools web_search and web_fetch)"
	webOnlyLeavesLine  = "This prompt's text may be sent to any website the run visits."
)

func contextLines(value string) []string {
	if value == ContextWebOnly {
		return []string{contextWebOnlyLine, webOnlyLeavesLine}
	}
	return []string{contextFullLine}
}

func previewOf(spec *JobSpec, route string) operation.Preview {
	lines := []string{
		fmt.Sprintf("Schedule: %s (%s)", spec.CronExpression, spec.Timezone),
		"Name: " + spec.Name,
	}
	if spec.WorkflowID != "" {
		lines = append(lines, "Starts workflow: "+spec.WorkflowID)
		if spec.WorkflowInput != "" {
			lines = append(lines, "Workflow input: "+spec.WorkflowInput)
		}
	} else {
		lines = append(lines, "Runs prompt: "+spec.Prompt)
		lines = append(lines, contextLines(spec.Context)...)
	}
	lines = append(lines,
		"Channel: "+spec.OriginChannel,
		"Destination alias: "+spec.OriginDestination,
	)
	if route != "" {
		lines = append(lines, "Delivers to: "+route)
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return operation.Preview{Lines: lines, Digest: hex.EncodeToString(sum[:])}
}

func (o *Operations) add(ctx context.Context, call *operation.Call) (operation.Result, error) {
	if o.starter == nil {
		return operation.Result{}, fault.New(ErrorCodeInvalidArgument, "schedule starter must not be nil")
	}
	spec, err := o.spec(call)
	if err != nil {
		return operation.Result{}, err
	}
	if call.PreviewDigest != "" && previewOf(spec, o.routeOf(spec.OriginDestination)).Digest != call.PreviewDigest {
		return operation.Result{}, operation.NotPerformed(operation.ErrorCodePreviewStale,
			operation.NewError(operation.ErrorCodePreviewStale, "the owner destination changed after the preview"))
	}
	if err := o.checkWorkflow(ctx, spec); err != nil {
		return operation.Result{}, err
	}
	return call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		job, err := o.service.Create(ctx, spec)
		if err != nil {
			return operation.Result{}, err
		}
		if err := StartNext(ctx, &NextRequest{Starter: o.starter, Job: &job, After: o.now().UTC()}); err != nil {
			return operation.Result{}, err
		}
		return encodeResult(map[string]any{"id": job.ID})
	})
}

func encodeResult(data any) (operation.Result, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return operation.Result{}, fault.Wrap(ErrorCodeInvalidArgument, "encode the operation result", err)
	}
	return operation.Result{Data: encoded}, nil
}

func (o *Operations) setState(state string) operation.Handler {
	return func(ctx context.Context, call *operation.Call) (operation.Result, error) {
		id, err := o.jobID(call)
		if err != nil {
			return operation.Result{}, err
		}
		return call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
			if err := o.service.SetState(ctx, id, state); err != nil {
				return operation.Result{}, err
			}
			return encodeResult(map[string]any{"id": id, "state": state})
		})
	}
}

func (o *Operations) resume(ctx context.Context, call *operation.Call) (operation.Result, error) {
	if o.starter == nil {
		return operation.Result{}, fault.New(ErrorCodeInvalidArgument, "schedule starter must not be nil")
	}
	id, err := o.jobID(call)
	if err != nil {
		return operation.Result{}, err
	}
	return call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		if err := o.service.SetState(ctx, id, JobActive); err != nil {
			return operation.Result{}, err
		}
		job, err := o.service.Get(ctx, id)
		if err != nil {
			return operation.Result{}, err
		}
		if err := StartNext(ctx, &NextRequest{Starter: o.starter, Job: &job, After: o.now().UTC()}); err != nil {
			return operation.Result{}, err
		}
		return encodeResult(map[string]any{"id": id, "state": JobActive})
	})
}

func (o *Operations) runNow(ctx context.Context, call *operation.Call) (operation.Result, error) {
	if o.starter == nil {
		return operation.Result{}, fault.New(ErrorCodeInvalidArgument, "schedule starter must not be nil")
	}
	id, err := o.jobID(call)
	if err != nil {
		return operation.Result{}, err
	}
	return call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		occurrence, err := o.service.RunNow(ctx, id, o.now().UTC())
		if err != nil {
			return operation.Result{}, err
		}
		if err := StartFire(ctx, &FireRequest{Starter: o.starter, JobID: occurrence.JobID, Instant: occurrence.ScheduledForUTC}); err != nil {
			return operation.Result{}, err
		}
		return encodeResult(map[string]any{"id": id, "occurrence": occurrence.ID})
	})
}

type listedJob struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Schedule    string  `json:"schedule"`
	Timezone    string  `json:"timezone"`
	State       string  `json:"state"`
	Context     string  `json:"context"`
	Tainted     bool    `json:"tainted"`
	TaintSource string  `json:"taint_source"`
	NextFire    *string `json:"next_fire"`
}

func (o *Operations) list(ctx context.Context, _ *operation.Call) (operation.Result, error) {
	jobs, err := o.store.ListJobs(ctx, listedJobsLimit)
	if err != nil {
		return operation.Result{}, err
	}
	now := o.now().UTC()
	rows := make([]listedJob, 0, len(jobs))
	for i := range jobs {
		row := listedJob{
			ID: jobs[i].ID, Name: jobs[i].Name, Schedule: jobs[i].CronExpression, Timezone: jobs[i].Timezone, State: jobs[i].State,
			Context: listedContext(&jobs[i]), Tainted: jobs[i].Tainted, TaintSource: jobs[i].TaintSource,
		}
		if jobs[i].State == JobActive {
			if fire, err := o.service.NextFire(ctx, jobs[i].ID, now); err == nil {
				next := fire.Format(time.RFC3339)
				row.NextFire = &next
			}
		}
		rows = append(rows, row)
	}
	if err := ctx.Err(); err != nil {
		return operation.Result{}, err
	}
	return encodeResult(map[string]any{"rows": rows})
}

func listedContext(job *Job) string {
	if job.WorkflowID != "" {
		return contextNone
	}
	return job.Context
}

type listedRun struct {
	ID           string `json:"id"`
	ScheduledFor string `json:"scheduled_for"`
	State        string `json:"state"`
	Turn         string `json:"turn"`
}

func (o *Operations) runs(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in jobInput
	if err := decodeInput(call, &in); err != nil {
		return operation.Result{}, err
	}
	if strings.TrimSpace(in.ID) == "" {
		return operation.Result{}, usage("the job id must not be blank")
	}
	items, err := o.store.Occurrences(ctx, in.ID, in.Limit)
	if err != nil {
		return operation.Result{}, err
	}
	rows := make([]listedRun, 0, len(items))
	for i := range items {
		rows = append(rows, listedRun{
			ID: items[i].ID, ScheduledFor: items[i].ScheduledForUTC.Format(time.RFC3339),
			State: items[i].State, Turn: items[i].TurnID,
		})
	}
	if err := ctx.Err(); err != nil {
		return operation.Result{}, err
	}
	return encodeResult(map[string]any{"rows": rows})
}

func validateWorkflowInput(input json.RawMessage) error {
	if len(input) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	var parsed struct {
		Objective string `json:"objective"`
	}
	if err := decoder.Decode(&parsed); err != nil {
		return usage("input must be a JSON object with an optional objective")
	}
	return nil
}

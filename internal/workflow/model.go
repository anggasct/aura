package workflow

import "time"

// Kind enumerates the step executor kinds.
type Kind string

const (
	KindAgent    Kind = "agent"
	KindTool     Kind = "tool"
	KindWait     Kind = "wait"
	KindApproval Kind = "approval"
)

// Source marks where a spec came from.
type Source string

const (
	SourceDefined   Source = "defined"
	SourceComposed  Source = "composed"
	SourceGenerated Source = "generated"
)

func (s Source) valid() bool {
	switch s {
	case SourceDefined, SourceComposed, SourceGenerated:
		return true
	default:
		return false
	}
}

// Spec is one declarative workflow definition.
type Spec struct {
	ID      string     `json:"id"`
	Goal    string     `json:"goal"`
	Version int        `json:"version"`
	Source  Source     `json:"source"`
	Steps   []StepSpec `json:"steps"`
}

// StepSpec declares one step: executor, dependencies, condition, bounds.
type StepSpec struct {
	ID        string        `json:"id"`
	Executor  ExecutorSpec  `json:"executor"`
	DependsOn []string      `json:"depends_on,omitempty"`
	Condition *string       `json:"condition,omitempty"`
	Timeout   time.Duration `json:"timeout"`
	Retry     RetryPolicy   `json:"retry"`
}

// ExecutorSpec configures the step executor.
type ExecutorSpec struct {
	Kind                 Kind     `json:"kind"`
	AgentID              *string  `json:"agent_id,omitempty"`
	RequiredCapabilities []string `json:"requires,omitempty"`
	ToolID               *string  `json:"tool,omitempty"`
	Event                *string  `json:"event,omitempty"`
}

// RetryPolicy bounds per-step re-execution; zero attempts means no retry.
type RetryPolicy struct {
	Attempts int           `json:"attempts"`
	Backoff  time.Duration `json:"backoff"`
}

const (
	maxRetryAttempts = 5
	maxRetryBackoff  = 10 * time.Minute
)

// Run state machine values mirrored by the schema CHECK constraints.
const (
	RunQueued     = "queued"
	RunRunning    = "running"
	RunSuspended  = "suspended"
	RunSucceeded  = "succeeded"
	RunFailed     = "failed"
	RunCancelled  = "cancelled"
	StepPending   = "pending"
	StepReady     = "ready"
	StepRunning   = "running"
	StepSucceeded = "succeeded"
	StepFailed    = "failed"
	StepSkipped   = "skipped"
)

// RunInput carries caller-supplied resources for one run; resources are
// never assumed.
type RunInput struct {
	Objective   string            `json:"objective,omitempty"`
	Resources   []ResourceRef     `json:"resources,omitempty"`
	Artifacts   []ArtifactRef     `json:"artifacts,omitempty"`
	Permissions []string          `json:"permissions,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// ResourceRef points at one external resource supplied per run.
type ResourceRef struct {
	Kind string `json:"kind"`
	URI  string `json:"uri"`
}

// ArtifactRef points at one content-addressed artifact.
type ArtifactRef struct {
	Digest string `json:"digest"`
	URI    string `json:"uri"`
}

// ExecutionContext is the explicit execution context an agent step runs
// with.
type ExecutionContext struct {
	Objective   string
	Resources   []ResourceRef
	Artifacts   []ArtifactRef
	Permissions []string
	Metadata    map[string]string
}

// RunSummary is one run row projection.
type RunSummary struct {
	ID                string
	DefinitionID      string
	DefinitionVersion int
	DurableKey        string
	Goal              string
	Status            string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// StepRun is one step row projection.
type StepRun struct {
	RunID                string
	StepID               string
	Status               string
	Attempt              int
	StartedAt            *time.Time
	EndedAt              *time.Time
	Output               []byte
	OutputArtifactDigest string
	ErrorCode            string
	UpdatedAt            time.Time
}

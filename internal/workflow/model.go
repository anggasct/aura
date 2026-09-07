package workflow

import "time"

type Kind string

const (
	KindAgent    Kind = "agent"
	KindTool     Kind = "tool"
	KindWait     Kind = "wait"
	KindApproval Kind = "approval"
)

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

type Spec struct {
	ID      string     `json:"id"`
	Goal    string     `json:"goal"`
	Version int        `json:"version"`
	Source  Source     `json:"source"`
	Steps   []StepSpec `json:"steps"`
}

type StepSpec struct {
	ID        string        `json:"id"`
	Executor  ExecutorSpec  `json:"executor"`
	DependsOn []string      `json:"depends_on,omitempty"`
	Condition *string       `json:"condition,omitempty"`
	Timeout   time.Duration `json:"timeout"`
	Retry     RetryPolicy   `json:"retry"`
}

type ExecutorSpec struct {
	Kind                 Kind     `json:"kind"`
	AgentID              *string  `json:"agent_id,omitempty"`
	RequiredCapabilities []string `json:"requires,omitempty"`
	ToolID               *string  `json:"tool,omitempty"`
	Event                *string  `json:"event,omitempty"`
}

type RetryPolicy struct {
	Attempts int           `json:"attempts"`
	Backoff  time.Duration `json:"backoff"`
}

const (
	maxRetryAttempts = 5
	maxRetryBackoff  = 10 * time.Minute
)

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

type RunInput struct {
	Objective   string            `json:"objective,omitempty"`
	Resources   []ResourceRef     `json:"resources,omitempty"`
	Artifacts   []ArtifactRef     `json:"artifacts,omitempty"`
	Permissions []string          `json:"permissions,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type ResourceRef struct {
	Kind string `json:"kind"`
	URI  string `json:"uri"`
}

type ArtifactRef struct {
	Digest string `json:"digest"`
	URI    string `json:"uri"`
}

type ExecutionContext struct {
	Objective   string
	Resources   []ResourceRef
	Artifacts   []ArtifactRef
	Permissions []string
	Metadata    map[string]string
}

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

package workflow

import (
	"encoding/json"
	"time"

	"github.com/anggasct/aura/internal/platform/principal"
)

type Kind string

const (
	KindAgent    Kind = "agent"
	KindTool     Kind = "tool"
	KindWait     Kind = "wait"
	KindApproval Kind = "approval"
	KindWorkflow Kind = "workflow"
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
	Name    string     `json:"name,omitempty"`
	Goal    string     `json:"goal"`
	Version int        `json:"version"`
	Source  Source     `json:"source"`
	RunAs   string     `json:"run_as,omitempty"`
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
	Kind        Kind            `json:"kind"`
	AgentID     *string         `json:"agent_id,omitempty"`
	Prompt      string          `json:"prompt,omitempty"`
	Tools       []string        `json:"tools,omitempty"`
	Output      string          `json:"output,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	ToolID      *string         `json:"tool,omitempty"`
	ToolArgs    json.RawMessage `json:"args,omitempty"`
	Event       *string         `json:"event,omitempty"`
	Source      *string         `json:"source,omitempty"`
	ExternalRef *string         `json:"external_ref,omitempty"`
	WorkflowID  *string         `json:"workflow,omitempty"`
}

const (
	OutputText = "text"
	OutputJSON = "json"

	DefaultAgentMaxTokens = 50_000
	MaxAgentMaxTokens     = 200_000
)

func (e *ExecutorSpec) applyAgentDefaults() {
	if e.Output == "" {
		e.Output = OutputText
	}
	if e.MaxTokens == nil {
		budget := DefaultAgentMaxTokens
		e.MaxTokens = &budget
	}
}

func (e *ExecutorSpec) TokenBudget() int {
	if e.MaxTokens == nil {
		return DefaultAgentMaxTokens
	}
	return *e.MaxTokens
}

func (e *ExecutorSpec) OutputMode() string {
	if e.Output == "" {
		return OutputText
	}
	return e.Output
}

func (e *ExecutorSpec) hasAgentFields() bool {
	return e.AgentID != nil || e.Prompt != "" || len(e.Tools) > 0 || e.Output != "" || e.MaxTokens != nil
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

type RunOrigin struct {
	PrincipalID    string
	PrincipalClass string
	AgentID        string
	SessionID      string
	Reply          ReplyTarget
	Tainted        bool
}

type ReplyTarget struct {
	Channel   string
	Account   string
	ChannelID string
	ThreadID  string
}

func (t *ReplyTarget) IsZero() bool {
	return t.Channel == "" && t.Account == "" && t.ChannelID == "" && t.ThreadID == ""
}

func (t *ReplyTarget) Validate() error {
	if t.IsZero() {
		return nil
	}
	if t.Channel != principal.ChannelDiscord && t.Channel != principal.ChannelTelegram {
		return codedError(ErrorCodeInvalidArgument, "reply target channel must be discord or telegram")
	}
	if t.Account == "" || t.ChannelID == "" {
		return codedError(ErrorCodeInvalidArgument, "reply target needs a channel, an account and a channel id together")
	}
	return nil
}

func (o *RunOrigin) Complete() bool {
	if o == nil || o.PrincipalID == "" || o.AgentID == "" {
		return false
	}
	switch principal.Class(o.PrincipalClass) {
	case principal.ClassOwner, principal.ClassTrusted, principal.ClassPublic:
		return true
	}
	return false
}

type RunSummary struct {
	Origin            RunOrigin
	ParentRunID       string
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
	Tainted              bool
	Private              bool
}

package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/runtime/adk"
	"github.com/anggasct/aura/internal/secret"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/toolbroker"
	execTool "github.com/anggasct/aura/internal/tools/exec"
	fetchTool "github.com/anggasct/aura/internal/tools/fetch"
	filesystemTool "github.com/anggasct/aura/internal/tools/filesystem"
	searchTool "github.com/anggasct/aura/internal/tools/search"
)

type builtinToolExecutor struct {
	broker  *toolbroker.Broker
	journal *effect.Journal
}

// effectPublisherFunc adapts a plain publish function to the effect journal's
// EventPublisher so the runtime publisher can be forwarded.
type effectPublisherFunc func(*store.RuntimeEvent)

func (f effectPublisherFunc) Publish(ev *store.RuntimeEvent) { f(ev) }

// SetEventPublisher forwards the runtime event publisher to the effect
// journal so tool requests are published as they become durable, before the
// provider runs.
func (e *builtinToolExecutor) SetEventPublisher(publish func(*store.RuntimeEvent)) {
	if publish == nil || e.journal == nil {
		return
	}
	e.journal.SetEventPublisher(effectPublisherFunc(publish))
}

func newBuiltinToolExecutor(cfg *config.Config, db *sql.DB, logger *slog.Logger, observer toolbroker.Observer, decider toolbroker.ApprovalDecider) (*builtinToolExecutor, error) {
	if cfg == nil || cfg.Tools == nil {
		return nil, errors.New("tools configuration is required")
	}
	if db == nil {
		return nil, errors.New("storage database is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	toolsCfg := cfg.Tools
	execAdapter, err := execTool.New(execTool.Options{
		Workspace:      toolsCfg.Workspace,
		Timeout:        time.Duration(toolsCfg.Exec.Timeout),
		MaxStdoutBytes: toolsCfg.Exec.MaxStdoutBytes,
		MaxStderrBytes: toolsCfg.Exec.MaxStderrBytes,
	})
	if err != nil {
		return nil, err
	}
	filesystemAdapters, err := filesystemTool.New(filesystemTool.Options{Workspace: toolsCfg.Workspace})
	if err != nil {
		return nil, err
	}
	fetchAdapter, err := fetchTool.New(fetchTool.Options{
		Timeout:         time.Duration(toolsCfg.Fetch.Timeout),
		MaxRedirects:    toolsCfg.Fetch.MaxRedirects,
		MaxEncodedBytes: toolsCfg.Fetch.MaxEncodedBytes,
		MaxDecodedBytes: toolsCfg.Fetch.MaxDecodedBytes,
	})
	if err != nil {
		return nil, err
	}
	searchAdapter, err := searchTool.New(&searchTool.Options{
		Provider:      toolsCfg.WebSearch.Provider,
		CredentialRef: toolsCfg.WebSearch.CredentialRef,
		Timeout:       time.Duration(toolsCfg.Fetch.Timeout),
		MaxResults:    toolsCfg.WebSearch.MaxResults,
		MaxBodyBytes:  toolsCfg.Fetch.MaxDecodedBytes,
	})
	if err != nil {
		return nil, err
	}
	adapters := filesystemAdapters
	adapters["exec@v1"] = execAdapter
	adapters["web_fetch@v1"] = fetchAdapter
	adapters["web_search@v1"] = searchAdapter
	_, artifactRoot, _, err := storagePaths(cfg)
	if err != nil {
		return nil, err
	}
	journal, err := effect.NewJournal(db, effect.Options{Logger: logger})
	if err != nil {
		return nil, err
	}
	effects, err := effect.NewExecutor(journal)
	if err != nil {
		return nil, err
	}
	broker, err := toolbroker.New(&toolbroker.Options{
		Adapters:             adapters,
		Secrets:              configuredToolSecrets(cfg),
		MaxInlineResultBytes: toolsCfg.MaxInlineResultBytes,
		Artifacts:            store.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota)),
		Effects:              effects,
		Observer:             observer,
		ApprovalDecider:      decider,
	})
	if err != nil {
		return nil, err
	}
	return &builtinToolExecutor{broker: broker, journal: journal}, nil
}

func configuredToolSecrets(cfg *config.Config) []string {
	if cfg == nil || cfg.Tools == nil {
		return nil
	}
	ref := cfg.Tools.WebSearch.CredentialRef
	var source secret.Reference
	switch {
	case strings.HasPrefix(ref, "env://"):
		source.Env = strings.TrimPrefix(ref, "env://")
	case strings.HasPrefix(ref, "file://"):
		source.File = strings.TrimPrefix(ref, "file://")
	default:
		return nil
	}
	value, err := source.Resolve()
	if err != nil || value == "" {
		return nil
	}
	return []string{value}
}

func (e *builtinToolExecutor) Definitions() []runtimeadk.BuiltinToolDefinition {
	definitions := e.broker.Definitions()
	result := make([]runtimeadk.BuiltinToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, runtimeadk.BuiltinToolDefinition{
			Name:                 definition.Name,
			Version:              definition.Version,
			Description:          "Aura built-in " + definition.Name + " tool",
			Schema:               definition.Schema,
			RequiredCapabilities: definition.RequiredCapabilities,
		})
	}
	return result
}

func (e *builtinToolExecutor) Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error) {
	if request == nil {
		return approval.PolicyDecision{}, approval.Errorf(approval.ErrorCodeInvalidArgument, "tool request must not be nil")
	}
	return e.broker.Evaluate(ctx, &toolbroker.ToolRequest{
		RequestID: request.RequestID, TurnID: request.TurnID, SessionID: request.SessionID,
		PrincipalID: request.PrincipalID, ToolName: request.ToolName, ToolVersion: request.ToolVersion,
		Arguments: request.Arguments, RequestDigest: request.RequestDigest, Capabilities: request.Capabilities,
		Trust: request.Trust, Deadline: request.Deadline, IdempotencyKey: request.IdempotencyKey,
	})
}

func (e *builtinToolExecutor) Execute(ctx context.Context, request *runtimeadk.BuiltinToolRequest) (json.RawMessage, error) {
	if request == nil {
		return nil, &runtime.Error{Code: runtime.ErrorCodeInvalidArgument, Detail: "builtin tool request must not be nil"}
	}
	result, err := e.broker.Execute(ctx, &toolbroker.ToolRequest{
		RequestID: request.RequestID, TurnID: request.TurnID, SessionID: request.SessionID,
		PrincipalID: request.PrincipalID, ToolName: request.ToolName, ToolVersion: request.ToolVersion,
		Arguments: request.Arguments, Capabilities: request.Capabilities, Trust: approval.TrustLabel(request.Trust),
		Deadline: request.Deadline, IdempotencyKey: request.IdempotencyKey,
		EventSequence: request.EventSequence, EventInvocation: request.EventInvocation,
		EventBranch: request.EventBranch, EventAuthor: request.EventAuthor,
	})
	if err != nil {
		return nil, err
	}
	return result.Output, nil
}

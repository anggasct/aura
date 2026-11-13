package toolsbuiltin

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/config"
	fetchTool "github.com/anggasct/aura/internal/modules/fetch"
	filesystemTool "github.com/anggasct/aura/internal/modules/filesystem"
	searchTool "github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/modules/shellexec"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/adk"
	"github.com/anggasct/aura/internal/store"
)

type Executor struct {
	broker    *toolbroker.Broker
	journal   *effect.Journal
	handoff   *handoffHolder
	emails    *emailHolder
	workflows *workflowHolder
	knowledge *knowledgeHolder
	code      *codeHolder
	artifacts store.ArtifactStore
	host      []string
}

type effectPublisherFunc func(*store.RuntimeEvent)

func (f effectPublisherFunc) Publish(ev *store.RuntimeEvent) { f(ev) }

func (e *Executor) SetEventPublisher(publish func(*store.RuntimeEvent)) {
	if publish == nil || e.journal == nil {
		return
	}
	e.journal.SetEventPublisher(effectPublisherFunc(publish))
}

type Option func(*toolbroker.Options)

func WithApprovalReachable(reachable func(sessionID string) bool) Option {
	return func(options *toolbroker.Options) { options.ApprovalReachable = reachable }
}

func New(cfg *config.Config, db *sql.DB, artifactRoot string, capabilities capability.Report, logger *slog.Logger, observer toolbroker.Observer, decider toolbroker.ApprovalDecider, access toolbroker.AccessPolicy, opts ...Option) (*Executor, error) {
	if cfg == nil || cfg.Tools == nil {
		return nil, errors.New("tools configuration is required")
	}
	if db == nil {
		return nil, errors.New("storage database is required")
	}
	if artifactRoot == "" {
		return nil, errors.New("artifact root is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	handoff := &handoffHolder{}
	emails := &emailHolder{}
	workflows := &workflowHolder{}
	knowledge := &knowledgeHolder{}
	code := &codeHolder{}
	adapters, err := builtinAdapters(cfg.Tools, handoff, emails, workflows, knowledge, code)
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
	host := capabilities.Active()
	artifacts := store.NewArtifactStore(db, artifactRoot, int64(cfg.Storage.ArtifactQuota))
	brokerOptions := &toolbroker.Options{
		Adapters:             adapters,
		Secrets:              configuredToolSecrets(cfg),
		MaxInlineResultBytes: cfg.Tools.MaxInlineResultBytes,
		Artifacts:            artifacts,
		Effects:              effects,
		Observer:             observer,
		ApprovalDecider:      decider,
		Access:               access,
		ApprovalGuards:       map[string]toolbroker.ApprovalGuard{tools.EmailSendToolName: emails.guard},
		HostCapabilities:     host,
		ArgumentChecks:       map[string]toolbroker.ArgumentCheck{codeKey(): code.check},
	}
	for _, apply := range opts {
		apply(brokerOptions)
	}
	broker, err := toolbroker.New(brokerOptions)
	if err != nil {
		return nil, err
	}
	return &Executor{broker: broker, journal: journal, handoff: handoff, emails: emails, workflows: workflows, knowledge: knowledge, code: code, artifacts: artifacts, host: host}, nil
}

func NewChannelEffects(db *sql.DB, logger *slog.Logger) (*effect.Executor, error) {
	if db == nil {
		return nil, errors.New("storage database is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	journal, err := effect.NewJournal(db, effect.Options{Logger: logger})
	if err != nil {
		return nil, err
	}
	return effect.NewExecutor(journal)
}

func builtinAdapters(toolsCfg *config.Tools, handoff *handoffHolder, emails *emailHolder, workflows *workflowHolder, knowledge *knowledgeHolder, code *codeHolder) (map[string]toolbroker.Adapter, error) {
	if toolsCfg == nil {
		return nil, errors.New("tools configuration is required")
	}
	execAdapter, err := shellexec.New(shellexec.Options{
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
	searchOptions := &searchTool.Options{
		Provider:      toolsCfg.WebSearch.Provider,
		CredentialRef: toolsCfg.WebSearch.CredentialRef,
		Timeout:       time.Duration(toolsCfg.Fetch.Timeout),
		MaxResults:    toolsCfg.WebSearch.MaxResults,
		MaxBodyBytes:  toolsCfg.Fetch.MaxDecodedBytes,
	}
	if toolsCfg.WebSearch.Endpoint != "" {
		endpoint, err := toolsCfg.WebSearch.SearchEndpoint()
		if err != nil {
			return nil, fmt.Errorf("web search endpoint: %w", err)
		}
		searchOptions.Endpoint = endpoint.URL()
		if endpoint.Loopback {
			searchOptions.LoopbackAuthority = endpoint.Authority()
		}
	}
	searchAdapter, err := searchTool.New(searchOptions)
	if err != nil {
		return nil, err
	}
	adapters := filesystemAdapters
	adapters["exec@v1"] = execAdapter
	adapters["web_fetch@v1"] = fetchAdapter
	adapters["web_search@v1"] = searchAdapter
	adapters[tools.HandoffToolName+"@"+tools.Version] = handoff.adapter
	adapters[tools.EmailSendToolName+"@"+tools.Version] = emails.adapter
	adapters[tools.KBSearchToolName+"@"+tools.Version] = knowledge.adapter
	adapters[codeKey()] = code.adapter
	maps.Copy(adapters, workflows.adapters())
	return adapters, nil
}

func DefinitionNames() []string {
	registry := tools.DefinitionsByKey()
	names := make([]string, 0, len(registry))
	for _, definition := range registry {
		names = append(names, definition.Name)
	}
	slices.Sort(names)
	return names
}

func CatalogDefinitions() []runtimeadk.BuiltinToolDefinition {
	builtins := tools.Builtins()
	out := make([]runtimeadk.BuiltinToolDefinition, 0, len(builtins))
	for i := range builtins {
		out = append(out, runtimeadk.BuiltinToolDefinition{
			Name: builtins[i].Name, Version: builtins[i].Version, Description: "Aura built-in " + builtins[i].Name + " tool",
			Schema: builtins[i].Schema, RequiredCapabilities: builtins[i].RequiredCapabilities, OptIn: builtins[i].OptIn,
		})
	}
	return out
}

func EffectfulToolNames() []string {
	registry := tools.DefinitionsByKey()
	names := make([]string, 0, len(registry))
	for _, definition := range registry {
		if definition.RequiresApproval || definition.Effectful {
			names = append(names, definition.Name)
		}
	}
	slices.Sort(names)
	return names
}

func StepToolNames(personaTools []string) []string {
	orchestration := map[string]bool{}
	builtins := tools.Builtins()
	for i := range builtins {
		if builtins[i].Orchestration {
			orchestration[builtins[i].Name] = true
		}
	}
	return slices.DeleteFunc(slices.Clone(personaTools), func(name string) bool { return orchestration[name] })
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

func (e *Executor) Definitions() []runtimeadk.BuiltinToolDefinition {
	definitions := e.broker.Definitions()
	result := make([]runtimeadk.BuiltinToolDefinition, 0, len(definitions))
	for i := range definitions {
		definition := &definitions[i]
		if !e.offers(definition) {
			continue
		}
		description := cmp.Or(definition.Description, "Aura built-in "+definition.Name+" tool")
		plugin := ""
		if definition.Server != "" {
			description = "Tool " + definition.Name + " provided by the external server " + definition.Server
		}
		if definition.Plugin != nil {
			plugin = definition.Plugin.ID
			description = definition.Plugin.Description
		}
		result = append(result, runtimeadk.BuiltinToolDefinition{
			Name:                 definition.Name,
			Version:              definition.Version,
			Server:               definition.Server,
			Plugin:               plugin,
			Description:          description,
			Schema:               definition.Schema,
			RequiredCapabilities: definition.RequiredCapabilities,
			OptIn:                definition.OptIn,
		})
	}
	return result
}

func (e *Executor) offers(definition *tools.Definition) bool {
	for _, required := range definition.RequiredCapabilities {
		if !slices.Contains(e.host, required) {
			return false
		}
	}
	return true
}

func (e *Executor) Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error) {
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

func (e *Executor) Execute(ctx context.Context, request *runtimeadk.BuiltinToolRequest) (json.RawMessage, error) {
	if request == nil {
		return nil, &turn.Error{Code: turn.ErrorCodeInvalidArgument, Detail: "builtin tool request must not be nil"}
	}
	result, err := e.broker.Execute(ctx, &toolbroker.ToolRequest{
		RequestID: request.RequestID, TurnID: request.TurnID, SessionID: request.SessionID,
		PrincipalID: request.PrincipalID, PrincipalClass: request.PrincipalClass, AgentID: request.AgentID,
		ToolName: request.ToolName, ToolVersion: request.ToolVersion,
		Arguments: request.Arguments, Capabilities: request.Capabilities, Trust: approval.TrustLabel(request.Trust),
		Deadline: request.Deadline, IdempotencyKey: request.IdempotencyKey,
		AllocateEventSequence: true, EventInvocation: request.EventInvocation,
		EventBranch: request.EventBranch, EventAuthor: request.EventAuthor,
		Knowledge: slices.Clone(request.Knowledge), Tainted: request.Tainted,
	})
	if err != nil {
		return nil, err
	}
	return result.Output, nil
}

func (e *Executor) RegisterTool(definition *tools.Definition, adapter toolbroker.Adapter, rule *approval.Rule) error {
	if e == nil || e.broker == nil {
		return errors.New("executor broker is not initialized")
	}
	return e.broker.RegisterTool(definition, adapter, rule)
}

func (e *Executor) UnregisterTool(toolName, toolVersion string) {
	if e == nil || e.broker == nil {
		return
	}
	e.broker.UnregisterTool(toolName, toolVersion)
}

func (e *Executor) Broker() *toolbroker.Broker {
	if e == nil {
		return nil
	}
	return e.broker
}

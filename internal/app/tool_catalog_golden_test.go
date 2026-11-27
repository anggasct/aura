package app

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/email"
	"github.com/anggasct/aura/internal/modules/codeexec"
	"github.com/anggasct/aura/internal/modules/fetch"
	"github.com/anggasct/aura/internal/modules/filesystem"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/search"
	"github.com/anggasct/aura/internal/modules/shellexec"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

type goldenWorkflowController struct{}

func (goldenWorkflowController) SaveRoutine(context.Context, *workflow.Caller, string, string) (workflow.RoutineSummary, error) {
	return workflow.RoutineSummary{}, nil
}

func (goldenWorkflowController) Propose(context.Context, *workflow.Caller, json.RawMessage) (workflow.ProposalSummary, error) {
	return workflow.ProposalSummary{}, nil
}

func (goldenWorkflowController) List(context.Context, *workflow.Caller) ([]workflow.Info, error) {
	return nil, nil
}

func (goldenWorkflowController) Start(context.Context, *workflow.Caller, string, string) (workflow.RunState, error) {
	return workflow.RunState{}, nil
}

func (goldenWorkflowController) Status(context.Context, *workflow.Caller, string) (workflow.RunState, error) {
	return workflow.RunState{}, nil
}

func (goldenWorkflowController) Cancel(context.Context, *workflow.Caller, string) (workflow.RunState, error) {
	return workflow.RunState{}, nil
}

type catalogDefinition struct {
	Key                  string          `json:"key"`
	Name                 string          `json:"name"`
	Version              string          `json:"version"`
	Server               string          `json:"server"`
	Plugin               bool            `json:"plugin"`
	Schema               json.RawMessage `json:"schema"`
	RequiredCapabilities []string        `json:"required_capabilities"`
	RequiresApproval     bool            `json:"requires_approval"`
	Effectful            bool            `json:"effectful"`
	OptIn                bool            `json:"opt_in"`
	OwnerOptIn           bool            `json:"owner_opt_in"`
	ExternalSink         bool            `json:"external_sink"`
	NoStandingGrant      bool            `json:"no_standing_grant"`
	ExactDisplay         bool            `json:"exact_display"`
	Orchestration        bool            `json:"orchestration"`
	Display              []string        `json:"display"`
	HasValidator         bool            `json:"has_validator"`
}

type catalogRule struct {
	ToolName             string                `json:"tool_name"`
	ToolVersion          string                `json:"tool_version"`
	RequiresApproval     bool                  `json:"requires_approval"`
	RequiredCapabilities []string              `json:"required_capabilities"`
	AllowedTrust         []approval.TrustLabel `json:"allowed_trust"`
	Constraints          approval.Constraints  `json:"constraints"`
}

type toolCatalog struct {
	PolicyVersion string              `json:"policy_version"`
	Definitions   []catalogDefinition `json:"definitions"`
	Rules         []catalogRule       `json:"rules"`
}

func registerAllBuiltinToolProviders(t *testing.T, broker *toolbroker.Broker) {
	t.Helper()
	noop := func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: json.RawMessage(`{}`)}, nil
	}
	fetchMod, err := fetch.New(&fetch.Deps{})
	if err != nil {
		t.Fatalf("fetch.New: %v", err)
	}
	searchMod, err := search.New(&search.Deps{})
	if err != nil {
		t.Fatalf("search.New: %v", err)
	}
	workflowProvider, err := workflow.NewToolProvider(goldenWorkflowController{})
	if err != nil {
		t.Fatalf("workflow.NewToolProvider: %v", err)
	}
	providers := []toolbroker.ToolProvider{
		shellexec.NewToolProvider(noop),
		codeexec.NewToolProvider(noop, nil),
		filesystem.NewToolProvider(map[string]toolbroker.Adapter{
			filesystem.ReadFileToolName + "@" + tools.Version:  noop,
			filesystem.WriteFileToolName + "@" + tools.Version: noop,
			filesystem.ListDirToolName + "@" + tools.Version:   noop,
		}),
		fetchMod,
		searchMod,
		knowledge.NewToolProvider(nil),
		handoff.NewToolProvider(nil),
		email.NewToolProvider(),
		workflowProvider,
	}
	for i, provider := range providers {
		if err := broker.RegisterProvider(provider); err != nil {
			t.Fatalf("RegisterProvider[%d]: %v", i, err)
		}
	}
}

func TestToolCatalogGolden(t *testing.T) {
	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	registerAllBuiltinToolProviders(t, broker)

	definitions := broker.Definitions()
	if len(definitions) != 16 {
		t.Fatalf("registered definitions = %d, want 16", len(definitions))
	}
	if staticCount := len(appconfig.BuiltinToolDefinitions()); staticCount != len(definitions) {
		t.Fatalf("BuiltinToolDefinitions() = %d, want %d", staticCount, len(definitions))
	}

	catalog := toolCatalog{}
	for i := range definitions {
		definition := &definitions[i]
		if definition.Validator == nil {
			t.Fatalf("tool %s has nil Validator", definition.Key())
		}
		var schema map[string]any
		if err := json.Unmarshal(definition.Schema, &schema); err != nil {
			t.Fatalf("schema %s: %v", definition.Key(), err)
		}
		if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("schema %s has unexpected dialect", definition.Key())
		}
		catalog.Definitions = append(catalog.Definitions, catalogDefinition{
			Key:                  definition.Key(),
			Name:                 definition.Name,
			Version:              definition.Version,
			Server:               definition.Server,
			Plugin:               definition.Plugin != nil,
			Schema:               definition.Schema,
			RequiredCapabilities: definition.RequiredCapabilities,
			RequiresApproval:     definition.RequiresApproval,
			Effectful:            definition.Effectful,
			OptIn:                definition.OptIn,
			OwnerOptIn:           definition.OwnerOptIn,
			ExternalSink:         definition.ExternalSink,
			NoStandingGrant:      definition.NoStandingGrant,
			ExactDisplay:         definition.ExactDisplay,
			Orchestration:        definition.Orchestration,
			Display:              definition.Display,
			HasValidator:         false,
		})
	}
	slices.SortFunc(catalog.Definitions, func(a, b catalogDefinition) int {
		return cmp.Compare(a.Key, b.Key)
	})
	policy := broker.Policy()
	catalog.PolicyVersion = policy.Version
	for _, name := range slices.Sorted(maps.Keys(policy.Rules)) {
		rule := policy.Rules[name]
		catalog.Rules = append(catalog.Rules, catalogRule{
			ToolName:             rule.ToolName,
			ToolVersion:          rule.ToolVersion,
			RequiresApproval:     rule.RequiresApproval,
			RequiredCapabilities: rule.RequiredCapabilities,
			AllowedTrust:         rule.AllowedTrust,
			Constraints:          rule.Constraints,
		})
	}
	got, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		t.Fatalf("marshal catalog: %v", err)
	}
	got = append(got, '\n')
	goldenPath := filepath.Join("..", "platform", "toolbroker", "testdata", "golden", "tool-catalog.json")
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("tool catalog drifted from golden file\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

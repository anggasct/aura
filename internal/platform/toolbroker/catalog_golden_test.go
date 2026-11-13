package toolbroker

import (
	"bytes"
	"cmp"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/tools"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

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
	ExternalSink         bool            `json:"external_sink"`
	NoStandingGrant      bool            `json:"no_standing_grant"`
	ExactDisplay         bool            `json:"exact_display"`
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

func TestToolCatalogGolden(t *testing.T) {
	builtins := tools.Builtins()
	catalog := toolCatalog{}
	for i := range builtins {
		definition := &builtins[i]
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
			ExternalSink:         definition.ExternalSink,
			NoStandingGrant:      definition.NoStandingGrant,
			ExactDisplay:         definition.ExactDisplay,
			Display:              definition.Display,
			HasValidator:         definition.Validator != nil,
		})
	}
	slices.SortFunc(catalog.Definitions, func(a, b catalogDefinition) int {
		return cmp.Compare(a.Key, b.Key)
	})
	policy := DefaultPolicy()
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
	goldenPath := filepath.Join("testdata", "golden", "tool-catalog.json")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, got, 0o600); err != nil {
			t.Fatalf("rewrite golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("tool catalog drifted from golden file\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

//go:build linux

package toolsbuiltin

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
)

func TestDefinitionsCarryTheApprovedPluginDescription(t *testing.T) {
	db, err := store.OpenDB(t.Context(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := config.Default()
	cfg.Tools.Workspace = t.TempDir()
	dataRoot := t.TempDir()
	cfg.Storage.Path = dataRoot
	executor, err := New(&cfg, db, dataRoot, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	definition := &tools.Definition{
		Name: "plugin_weather_forecast", Version: "v1", Plugin: &tools.PluginRef{ID: "weather", Description: "Reviewed forecast text."},
		Schema: json.RawMessage(`{"type":"object"}`), RequiresApproval: true, Effectful: true,
	}
	rule := approval.Rule{ToolName: definition.Name, ToolVersion: "v1", RequiresApproval: true, AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput}}
	adapter := func(context.Context, *toolbroker.ToolRequest, approval.Constraints) (toolbroker.ToolResult, error) {
		return toolbroker.ToolResult{}, nil
	}
	if err := executor.Broker().RegisterTool(definition, adapter, &rule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	for _, listed := range executor.Definitions() {
		if listed.Name != definition.Name {
			continue
		}
		if listed.Plugin != "weather" || listed.Server != "" || listed.Description != "Reviewed forecast text." {
			t.Fatalf("listed = %+v", listed)
		}
		return
	}
	t.Fatal("plugin tool is not listed")
}

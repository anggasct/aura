package toolbroker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/tools"
)

func registerPluginTool(t *testing.T, harness *accessBroker, name, plugin string, requiresApproval bool) {
	t.Helper()
	definition := &tools.Definition{
		Name: name, Version: "v1", Plugin: &tools.PluginRef{ID: plugin, Description: "approved text"},
		Schema: json.RawMessage(`{"type":"object"}`), RequiresApproval: requiresApproval, Effectful: true,
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	rule := approval.Rule{
		ToolName: name, ToolVersion: "v1", RequiresApproval: requiresApproval,
		AllowedTrust: []approval.TrustLabel{approval.TrustOwnerInput, approval.TrustDerivedUntrusted},
	}
	adapter := func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		harness.calls[name]++
		return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}
	if err := harness.broker.RegisterTool(definition, adapter, &rule); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
}

func TestPublicPrincipalsNeverRunPluginToolsEvenInsideTheCeiling(t *testing.T) {
	access := teamAccess()
	access.ceilings[principal.ClassPublic] = append(access.ceilings[principal.ClassPublic], "plugin_weather", "plugin_weather_forecast")
	access.standing = true
	harness := newAccessBroker(t, access)
	registerPluginTool(t, harness, "plugin_weather_forecast", "weather", false)
	_, err := harness.broker.Execute(t.Context(), classifiedRequest("plugin_weather_forecast", `{}`, principal.ClassPublic))
	if classOf(err) != ResultToolTierDenied {
		t.Fatalf("public error = %v, want tool_tier_denied", err)
	}
	if harness.calls["plugin_weather_forecast"] != 0 || harness.prompts != 0 || access.lookups != 0 {
		t.Fatalf("calls = %d prompts = %d lookups = %d, want none", harness.calls["plugin_weather_forecast"], harness.prompts, access.lookups)
	}
}

func TestTrustedPluginToolsFollowThePrincipalCeiling(t *testing.T) {
	cases := map[string]struct {
		ceiling []string
		allowed bool
	}{
		"exact tool entry":                {[]string{"plugin_weather_forecast"}, true},
		"plugin entry covers its tools":   {[]string{"plugin_weather"}, true},
		"sibling tool entry":              {[]string{"plugin_weather_alerts"}, false},
		"other plugin entry":              {[]string{"plugin_mail"}, false},
		"longer plugin entry":             {[]string{"plugin_weather-pro"}, false},
		"mcp server entry of the same id": {[]string{"mcp_weather"}, false},
		"built-in tools only":             {[]string{"exec", "read_file"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			access := teamAccess()
			access.ceilings[principal.ClassTrusted] = tc.ceiling
			harness := newAccessBroker(t, access)
			registerPluginTool(t, harness, "plugin_weather_forecast", "weather", true)
			_, err := harness.broker.Execute(t.Context(), classifiedRequest("plugin_weather_forecast", `{}`, principal.ClassTrusted))
			if tc.allowed {
				if err != nil {
					t.Fatalf("Execute: %v", err)
				}
				return
			}
			if classOf(err) != ResultToolTierDenied {
				t.Fatalf("Execute error = %v, want tool_tier_denied", err)
			}
			if harness.calls["plugin_weather_forecast"] != 0 || harness.prompts != 0 {
				t.Fatalf("calls = %d prompts = %d, want neither", harness.calls["plugin_weather_forecast"], harness.prompts)
			}
		})
	}
}

func TestPluginToolsAskTheOwnerBeforeEveryCall(t *testing.T) {
	access := teamAccess()
	access.ceilings[principal.ClassTrusted] = []string{"plugin_weather"}
	harness := newAccessBroker(t, access)
	registerPluginTool(t, harness, "plugin_weather_forecast", "weather", true)
	if _, err := harness.broker.Execute(t.Context(), classifiedRequest("plugin_weather_forecast", `{}`, principal.ClassTrusted)); err != nil {
		t.Fatalf("approved trusted call: %v", err)
	}
	if harness.prompts != 1 || harness.calls["plugin_weather_forecast"] != 1 {
		t.Fatalf("prompts = %d calls = %d, want one owner card and one call", harness.prompts, harness.calls["plugin_weather_forecast"])
	}
	harness.approved = false
	denied := classifiedRequest("plugin_weather_forecast", `{}`, principal.ClassTrusted)
	denied.RequestID, denied.IdempotencyKey, denied.EventSequence = "request-2", "idempotency-2", 2
	if _, err := harness.broker.Execute(t.Context(), denied); classOf(err) != ResultApprovalRequired {
		t.Fatalf("declined call error = %v, want approval_required", err)
	}
	if harness.calls["plugin_weather_forecast"] != 1 {
		t.Fatalf("calls = %d, want the declined call not to run", harness.calls["plugin_weather_forecast"])
	}
}

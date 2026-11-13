package toolbroker

import (
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

func TestStandingApprovalAllowedMatchesTheStandingRule(t *testing.T) {
	cases := []struct {
		name       string
		definition tools.Definition
		guard      ApprovalGuard
		class      principal.Class
		tainted    bool
		toolName   string
		arguments  string
		want       bool
		wantCode   ResultClass
	}{
		{name: "trusted untainted unguarded call", class: principal.ClassTrusted, want: true},
		{name: "owner class call", class: principal.ClassOwner},
		{name: "public class call", class: principal.ClassPublic},
		{name: "tainted call", class: principal.ClassTrusted, tainted: true},
		{name: "tool without standing grants", definition: tools.Definition{NoStandingGrant: true}, class: principal.ClassTrusted},
		{name: "plugin field approval tool", definition: tools.Definition{Plugin: &tools.PluginRef{ID: "mailer"}}, class: principal.ClassTrusted},
		{name: "guarded call", guard: guardReturning(true, nil), class: principal.ClassTrusted},
		{name: "guard that does not require approval", guard: guardReturning(false, nil), class: principal.ClassTrusted, want: true},
		{name: "guard failure", guard: guardReturning(false, errors.New("route lookup failed")), class: principal.ClassTrusted, wantCode: ResultPolicyDenied},
		{name: "invalid argument", class: principal.ClassTrusted, arguments: `"text"`, wantCode: ResultInvalidArgument},
		{name: "unknown tool", class: principal.ClassTrusted, toolName: "missing_tool", wantCode: ResultPolicyDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newProbeRig(t, &tc.definition, tc.guard, true)
			request := func() *ToolRequest {
				arguments := tc.arguments
				if arguments == "" {
					arguments = `{"a":"b"}`
				}
				built := classifiedRequest(probeToolName, arguments, tc.class)
				built.Tainted = tc.tainted
				if tc.toolName != "" {
					built.ToolName = tc.toolName
				}
				return built
			}
			got, err := rig.broker.StandingApprovalAllowed(t.Context(), request())
			if code, _ := CodeOf(err); code != tc.wantCode {
				t.Fatalf("StandingApprovalAllowed error = %v, want code %q", err, tc.wantCode)
			}
			if got != tc.want {
				t.Fatalf("StandingApprovalAllowed = %t, want %t", got, tc.want)
			}
			if tc.wantCode != "" {
				return
			}
			_, _ = rig.broker.Execute(t.Context(), request())
			wantLookups := 0
			if tc.want {
				wantLookups = 1
			}
			if rig.access.lookups != wantLookups {
				t.Fatalf("Execute standing lookups = %d, want %d", rig.access.lookups, wantLookups)
			}
			if tc.want && len(rig.prompts) != 0 {
				t.Fatalf("Execute prompts = %d, want a covered call to run unprompted", len(rig.prompts))
			}
		})
	}
}

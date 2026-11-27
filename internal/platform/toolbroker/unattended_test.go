package toolbroker

import (
	"cmp"
	"context"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
)

func TestUnattendedRequestIsRefusedWithoutAsking(t *testing.T) {
	cases := []struct {
		name        string
		options     rigOptions
		request     func() *ToolRequest
		unattended  bool
		wantClass   ResultClass
		wantLookups int
		wantRuns    int32
	}{
		{
			name:    "an attended trusted call rides the standing grant",
			options: rigOptions{requiresApproval: true, access: standingAccess()},
			request: trustedProbeRequest, wantLookups: 1, wantRuns: 1,
		},
		{
			name:    "an unattended owner call needing approval is refused",
			options: rigOptions{requiresApproval: true},
			request: func() *ToolRequest { return probeRequest("probe") }, unattended: true, wantClass: ResultApprovalRequired,
		},
		{
			name:    "an unattended trusted call is refused before the standing grant lookup",
			options: rigOptions{requiresApproval: true, access: standingAccess()},
			request: trustedProbeRequest, unattended: true, wantClass: ResultApprovalRequired,
		},
		{
			name:    "an unattended tainted external sink call has no approver",
			options: rigOptions{sink: true, reachable: func(string) bool { return true }},
			request: func() *ToolRequest {
				request := classifiedRequest("probe", `{"query":"x"}`, principal.ClassOwner)
				request.Tainted = true
				return request
			},
			unattended: true, wantClass: ResultUntrustedInputNoApprover,
		},
		{
			name:    "an unattended call needing no approval still runs",
			request: func() *ToolRequest { return probeRequest("probe") }, unattended: true, wantRuns: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAuthorizationRig(t, &tc.options)
			request := tc.request()
			request.Unattended = tc.unattended
			got := outcomeOf(rig.broker.Execute(t.Context(), request))
			if want := cmp.Or(tc.wantClass, ResultOK); got.class != want {
				t.Fatalf("outcome class = %q (%s), want %q", got.class, got.text, want)
			}
			if prompts := rig.decider.asked(); len(prompts) != 0 {
				t.Errorf("decider calls = %d, want none", len(prompts))
			}
			if access, ok := tc.options.access.(*fakeAccess); ok && access.lookups != tc.wantLookups {
				t.Errorf("standing grant lookups = %d, want %d", access.lookups, tc.wantLookups)
			}
			if runs := rig.probe.calls.Load(); runs != tc.wantRuns {
				t.Errorf("tool runs = %d, want %d", runs, tc.wantRuns)
			}
		})
	}
}

func TestUnattendedRefusalRunsNoStandingGrantAction(t *testing.T) {
	hosted := startHostedRuntime(t)
	cases := []struct {
		name       string
		unattended bool
		want       []string
	}{
		{name: "attended call looks up the standing grant", want: []string{"call-1-0/policy", "call-1-0/standing-grant", "call-1-0/authorization", "call-1-0/execution"}},
		{name: "unattended call stops after the policy action", unattended: true, want: []string{"call-1-0/policy"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAuthorizationRig(t, &rigOptions{requiresApproval: true, access: standingAccess()})
			request := trustedProbeRequest()
			request.Unattended = tc.unattended
			key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				_, _ = rig.broker.Execute(inTurn(ctx, inv), request)
				return nil
			})
			var recorded []string
			for _, row := range hosted.actionRows(t, key) {
				recorded = append(recorded, row.key)
			}
			if !slices.Equal(recorded, tc.want) {
				t.Fatalf("action keys = %q, want %q", recorded, tc.want)
			}
		})
	}
}

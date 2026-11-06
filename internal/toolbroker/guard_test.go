package toolbroker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/capability/capabilitytest"
	"github.com/anggasct/aura/internal/principal"
)

func TestApprovalGuardDecidesWhetherAnOwnerOrTrustedCallNeedsApproval(t *testing.T) {
	cases := []struct {
		name         string
		class        principal.Class
		principalID  string
		standing     bool
		guard        ApprovalGuard
		accept       bool
		wantErrClass ResultClass
		wantPrompts  int
		wantCalls    int
		wantLookups  int
	}{
		{name: "unguarded owner call runs without a prompt", class: principal.ClassOwner, principalID: "local-owner", wantCalls: 1},
		{name: "guard false leaves the owner call unprompted", class: principal.ClassOwner, principalID: "local-owner", guard: guardReturning(false, nil), wantCalls: 1},
		{name: "guard true prompts the owner call and runs on accept", class: principal.ClassOwner, principalID: "local-owner", guard: guardReturning(true, nil), accept: true, wantPrompts: 1, wantCalls: 1},
		{name: "guard true prompts the owner call and stops on reject", class: principal.ClassOwner, principalID: "local-owner", guard: guardReturning(true, nil), wantPrompts: 1, wantErrClass: ResultApprovalRequired},
		{name: "guard true is not skipped by a standing grant", class: principal.ClassTrusted, principalID: "discord:111", standing: true, guard: guardReturning(true, nil), accept: true, wantPrompts: 1, wantCalls: 1},
		{name: "standing grant still covers an unguarded trusted call", class: principal.ClassTrusted, principalID: "discord:111", standing: true, guard: guardReturning(false, nil), wantCalls: 1, wantLookups: 1},
		{name: "guard failure denies without a prompt", class: principal.ClassOwner, principalID: "local-owner", guard: guardReturning(false, errors.New("route lookup failed")), wantErrClass: ResultPolicyDenied},
		{name: "guard failure denies a trusted call even with a standing grant", class: principal.ClassTrusted, principalID: "discord:111", standing: true, guard: guardReturning(false, errors.New("boom")), wantErrClass: ResultPolicyDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, prompts := 0, 0
			access := &fakeAccess{standing: tc.standing, ceilings: map[principal.Class][]string{principal.ClassTrusted: {"email_send"}}}
			options := &Options{
				Effects: newBrokerEffectExecutor(t),
				Access:  access,
				Adapters: map[string]Adapter{
					"email_send@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
						calls++
						return ToolResult{Output: json.RawMessage(`{"status":"sent"}`)}, nil
					},
				},
				ApprovalDecider: func(context.Context, *ApprovalPrompt) (bool, error) {
					prompts++
					return tc.accept, nil
				},
			}
			if tc.guard != nil {
				options.ApprovalGuards = map[string]ApprovalGuard{"email_send": tc.guard}
			}
			broker, err := New(options)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			request := brokerRequest("email_send", `{"to":"a@example.net","subject":"s","body":"b"}`)
			request.EventSequence = 1
			request.PrincipalClass = tc.class
			request.PrincipalID = tc.principalID
			request.AgentID = "team"
			_, err = broker.Execute(t.Context(), request)
			if class, _ := CodeOf(err); class != tc.wantErrClass {
				t.Fatalf("Execute error class = %q (%v), want %q", class, err, tc.wantErrClass)
			}
			if prompts != tc.wantPrompts || calls != tc.wantCalls || access.lookups != tc.wantLookups {
				t.Errorf("prompts=%d calls=%d lookups=%d, want %d %d %d", prompts, calls, access.lookups, tc.wantPrompts, tc.wantCalls, tc.wantLookups)
			}
		})
	}
}

func TestApprovalGuardsAreCopiedAtConstruction(t *testing.T) {
	guards := map[string]ApprovalGuard{"email_send": guardReturning(true, nil)}
	calls := 0
	broker, err := New(&Options{
		HostCapabilities: capabilitytest.Host(t), Effects: newBrokerEffectExecutor(t),
		ApprovalGuards: guards,
		Adapters: map[string]Adapter{
			"email_send@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				calls++
				return ToolResult{Output: json.RawMessage(`{}`)}, nil
			},
		},
		ApprovalDecider: func(context.Context, *ApprovalPrompt) (bool, error) { return false, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	delete(guards, "email_send")
	request := brokerRequest("email_send", `{"to":"a@example.net","subject":"s","body":"b"}`)
	request.EventSequence = 1
	if _, err := broker.Execute(t.Context(), request); classOf(err) != ResultApprovalRequired || calls != 0 {
		t.Fatalf("Execute = %v with %d calls, want the original guard to still require approval", err, calls)
	}
}

func guardReturning(required bool, err error) ApprovalGuard {
	return func(context.Context, *ToolRequest) (bool, error) { return required, err }
}

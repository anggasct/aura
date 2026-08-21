package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/capability"
)

type providerBroker struct {
	allow   bool
	execute bool
	handler approval.Handler
}

func (b *providerBroker) Evaluate(context.Context, *approval.ToolRequest) (approval.PolicyDecision, error) {
	if !b.allow {
		return approval.PolicyDecision{Outcome: approval.OutcomeDeny}, nil
	}
	return approval.PolicyDecision{Outcome: approval.OutcomeAllow}, nil
}

func (b *providerBroker) Grant(context.Context, *approval.ToolRequest, time.Duration) (approval.ApprovalGrant, error) {
	if !b.allow {
		return approval.ApprovalGrant{}, errors.New("provider test broker denied grant")
	}
	return approval.ApprovalGrant{GrantID: "grant-1", Nonce: "nonce-1"}, nil
}

func (b *providerBroker) Execute(ctx context.Context, request *approval.ToolRequest, _ *approval.ApprovalGrant) (approval.ToolResult, error) {
	if !b.execute {
		return approval.ToolResult{}, errors.New("provider test broker denied execution")
	}
	return b.handler(ctx, *request, approval.Constraints{})
}

func providerBrokerFactory(allow, execute bool) ProviderBrokerFactory {
	return func(handler approval.Handler) (approval.ToolBroker, error) {
		return &providerBroker{allow: allow, execute: execute, handler: handler}, nil
	}
}

func TestConformProvidersChecksAuthorizationBoundsAndSecretCanaries(t *testing.T) {
	descriptor := descriptorFixture()
	provider := func(output string, profile capability.Profile) *fakeProvider {
		return &fakeProvider{
			profile: ProviderProfile{Name: "provider", Capability: descriptor.Capability, BuildProfile: string(profile), MaxResultBytes: descriptor.MaxResultBytes},
			result:  ProviderResult{State: StateSucceeded, Output: json.RawMessage(output)},
		}
	}
	unauthorizedProvider := provider(`{"ok":true}`, capability.ProfileCore)
	unauthorizedInvoked := false
	unauthorizedProvider.invoked = &unauthorizedInvoked
	results := ConformProviders(context.Background(), []ProviderCase{
		{
			Name:          "unauthorized",
			Provider:      unauthorizedProvider,
			BrokerFactory: providerBrokerFactory(true, false),
			Descriptor:    descriptor,
			Arguments:     json.RawMessage(`{}`),
			Scope:         "workspace-1",
		},
		{
			Name:           "secret-leak",
			Provider:       provider(`{"value":"secret-canary"}`, capability.ProfileExecLinux),
			BrokerFactory:  providerBrokerFactory(true, true),
			Descriptor:     descriptor,
			Arguments:      json.RawMessage(`{}`),
			Scope:          "workspace-1",
			SecretCanaries: []string{"secret-canary"},
		},
		{
			Name:          "safe",
			Provider:      provider(`{"ok":true}`, capability.ProfileBrowserLinux),
			BrokerFactory: providerBrokerFactory(true, true),
			Descriptor:    descriptor,
			Arguments:     json.RawMessage(`{}`),
			Scope:         "workspace-1",
		},
		{
			Name:          "unbounded",
			Provider:      provider(strings.Repeat("x", descriptor.MaxResultBytes+1), capability.ProfileNativeLinux),
			BrokerFactory: providerBrokerFactory(true, true),
			Descriptor:    descriptor,
			Arguments:     json.RawMessage(`{}`),
			Scope:         "workspace-1",
		},
	})
	if len(results) != 4 || results[0].Name != "safe" || results[3].Name != "unbounded" {
		t.Fatalf("results = %+v, want sorted provider evidence", results)
	}
	if !results[0].Passed || results[1].Passed || results[2].Passed || results[3].Passed {
		t.Fatalf("results = %+v, want only safe provider to pass", results)
	}
	if unauthorizedInvoked {
		t.Fatal("provider executed after broker execution denial")
	}
}

func TestConformProvidersRejectsMissingAuthorizationBoundary(t *testing.T) {
	descriptor := descriptorFixture()
	cases := providerProfileCases(&descriptor)
	cases[0].BrokerFactory = nil
	results := ConformProviders(context.Background(), cases)
	if len(results) != len(cases) {
		t.Fatalf("results = %+v, want one result per profile", results)
	}
	found := false
	for _, result := range results {
		if result.Name == string(capability.ProfileCore) {
			found = true
			if result.Passed || result.Failure != "provider authorization boundary is missing" {
				t.Fatalf("results = %+v, want missing authorization boundary failure", results)
			}
		}
	}
	if !found {
		t.Fatalf("results = %+v, want core profile result", results)
	}
}

func TestConformProvidersCoversEverySupportedProfile(t *testing.T) {
	descriptor := descriptorFixture()
	cases := providerProfileCases(&descriptor)
	results := ConformProviders(context.Background(), cases)
	if len(results) != len(cases) {
		t.Fatalf("profile results = %d, want %d", len(results), len(cases))
	}
	for _, result := range results {
		if !result.Passed {
			t.Fatalf("profile %q failed conformance: %s", result.Name, result.Failure)
		}
	}
}

func TestConformProvidersRejectsIncompleteProfileSets(t *testing.T) {
	descriptor := descriptorFixture()
	complete := providerProfileCases(&descriptor)
	tests := []struct {
		name    string
		cases   []ProviderCase
		failure string
	}{
		{name: "empty", cases: nil, failure: "supported provider profile is missing"},
		{name: "missing", cases: complete[:len(complete)-1], failure: "supported provider profile is missing"},
	}
	duplicate := append([]ProviderCase{}, complete[:len(complete)-1]...)
	duplicateCase := complete[0]
	duplicateCase.Name = "core-duplicate"
	duplicate = append(duplicate, duplicateCase)
	tests = append(tests, struct {
		name    string
		cases   []ProviderCase
		failure string
	}{name: "duplicate", cases: duplicate, failure: "supported provider profile is duplicated"})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results := ConformProviders(context.Background(), test.cases)
			for _, result := range results {
				if strings.Contains(result.Failure, test.failure) {
					return
				}
			}
			t.Fatalf("results = %+v, want failure containing %q", results, test.failure)
		})
	}
}

func providerProfileCases(descriptor *Descriptor) []ProviderCase {
	cases := make([]ProviderCase, 0, len(capability.SupportedProfiles()))
	for _, profile := range capability.SupportedProfiles() {
		cases = append(cases, ProviderCase{
			Name:          string(profile),
			BrokerFactory: providerBrokerFactory(true, true),
			Descriptor:    *descriptor,
			Provider: &fakeProvider{
				profile: ProviderProfile{
					Name:           string(profile),
					Capability:     descriptor.Capability,
					BuildProfile:   string(profile),
					MaxResultBytes: descriptor.MaxResultBytes,
				},
				result: ProviderResult{State: StateSucceeded, Output: json.RawMessage(`{"ok":true}`)},
			},
			Arguments: json.RawMessage(`{}`),
			Scope:     "profile-scope",
		})
	}
	return cases
}

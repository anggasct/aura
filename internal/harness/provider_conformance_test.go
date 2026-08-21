package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/capability"
)

type providerBroker struct{ allow bool }

func (b providerBroker) Evaluate(context.Context, *approval.ToolRequest) (approval.PolicyDecision, error) {
	if !b.allow {
		return approval.PolicyDecision{Outcome: approval.OutcomeDeny}, nil
	}
	return approval.PolicyDecision{Outcome: approval.OutcomeAllow}, nil
}

func (providerBroker) Execute(context.Context, *approval.ToolRequest, *approval.ApprovalGrant) (approval.ToolResult, error) {
	return approval.ToolResult{}, errors.New("provider test broker does not execute")
}

func TestConformProvidersChecksAuthorizationBoundsAndSecretCanaries(t *testing.T) {
	descriptor := descriptorFixture()
	provider := func(output string) *fakeProvider {
		return &fakeProvider{
			profile: ProviderProfile{Name: "provider", Capability: descriptor.Capability, BuildProfile: "core", MaxResultBytes: descriptor.MaxResultBytes},
			result:  ProviderResult{State: StateSucceeded, Output: json.RawMessage(output)},
		}
	}
	results := ConformProviders(context.Background(), []ProviderCase{
		{
			Name:       "unauthorized",
			Provider:   provider(`{"ok":true}`),
			Broker:     providerBroker{},
			Descriptor: descriptor,
			Arguments:  json.RawMessage(`{}`),
			Scope:      "workspace-1",
		},
		{
			Name:           "secret-leak",
			Provider:       provider(`{"value":"secret-canary"}`),
			Broker:         providerBroker{allow: true},
			Descriptor:     descriptor,
			Arguments:      json.RawMessage(`{}`),
			Scope:          "workspace-1",
			SecretCanaries: []string{"secret-canary"},
		},
		{
			Name:       "safe",
			Provider:   provider(`{"ok":true}`),
			Broker:     providerBroker{allow: true},
			Descriptor: descriptor,
			Arguments:  json.RawMessage(`{}`),
			Scope:      "workspace-1",
		},
		{
			Name:       "unbounded",
			Provider:   provider(strings.Repeat("x", descriptor.MaxResultBytes+1)),
			Broker:     providerBroker{allow: true},
			Descriptor: descriptor,
			Arguments:  json.RawMessage(`{}`),
			Scope:      "workspace-1",
		},
	})
	if len(results) != 4 || results[0].Name != "safe" || results[3].Name != "unbounded" {
		t.Fatalf("results = %+v, want sorted provider evidence", results)
	}
	if !results[0].Passed || results[1].Passed || results[2].Passed || results[3].Passed {
		t.Fatalf("results = %+v, want only safe provider to pass", results)
	}
}

func TestConformProvidersRejectsMissingAuthorizationBoundary(t *testing.T) {
	descriptor := descriptorFixture()
	results := ConformProviders(context.Background(), []ProviderCase{{
		Name: "missing-broker", Provider: &fakeProvider{
			profile: ProviderProfile{Name: "provider", Capability: descriptor.Capability, BuildProfile: "core", MaxResultBytes: descriptor.MaxResultBytes},
			result:  ProviderResult{State: StateSucceeded, Output: json.RawMessage(`{"ok":true}`)},
		}, Descriptor: descriptor, Arguments: json.RawMessage(`{}`), Scope: "workspace-1",
	}})
	if len(results) != 1 || results[0].Passed || results[0].Failure != "provider authorization boundary is missing" {
		t.Fatalf("results = %+v, want missing authorization boundary failure", results)
	}
}

func TestConformProvidersCoversEverySupportedProfile(t *testing.T) {
	descriptor := descriptorFixture()
	cases := make([]ProviderCase, 0, len(capability.SupportedProfiles()))
	for _, profile := range capability.SupportedProfiles() {
		cases = append(cases, ProviderCase{
			Name:       string(profile),
			Broker:     providerBroker{allow: true},
			Descriptor: descriptor,
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

package harness

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/capability"
)

type ProviderBrokerFactory func(approval.Handler) (approval.ToolBroker, error)

type ProviderCase struct {
	Name           string
	Provider       Provider
	BrokerFactory  ProviderBrokerFactory
	Descriptor     Descriptor
	Arguments      json.RawMessage
	Scope          string
	SecretCanaries []string
}

type ProviderConformanceResult struct {
	Name    string
	Passed  bool
	Failure string
}

func ConformProviders(ctx context.Context, cases []ProviderCase) []ProviderConformanceResult {
	ordered := slices.Clone(cases)
	slices.SortFunc(ordered, func(left, right ProviderCase) int {
		return strings.Compare(left.Name, right.Name)
	})
	profiles := make(map[capability.Profile]int, len(ordered))
	var contractFailures []ProviderConformanceResult
	for i := range ordered {
		testCase := &ordered[i]
		if testCase.Provider == nil {
			contractFailures = append(contractFailures, ProviderConformanceResult{Name: testCase.Name, Failure: "provider is missing"})
			continue
		}
		profile := capability.Profile(testCase.Provider.Profile().BuildProfile)
		if !profile.Valid() {
			contractFailures = append(contractFailures, ProviderConformanceResult{Name: testCase.Name, Failure: "provider profile is unsupported"})
			continue
		}
		profiles[profile]++
	}
	for _, profile := range capability.SupportedProfiles() {
		switch profiles[profile] {
		case 0:
			contractFailures = append(contractFailures, ProviderConformanceResult{Name: "profile-set:" + string(profile), Failure: "supported provider profile is missing"})
		case 1:
		default:
			contractFailures = append(contractFailures, ProviderConformanceResult{Name: "profile-set:" + string(profile), Failure: "supported provider profile is duplicated"})
		}
	}
	if len(contractFailures) > 0 {
		return contractFailures
	}
	results := make([]ProviderConformanceResult, 0, len(ordered))
	for i := range ordered {
		testCase := &ordered[i]
		result := ProviderConformanceResult{Name: testCase.Name}
		request := ProviderRequest{Descriptor: testCase.Descriptor, Arguments: testCase.Arguments, Scope: testCase.Scope}
		if testCase.BrokerFactory == nil {
			result.Failure = "provider authorization boundary is missing"
			results = append(results, result)
			continue
		}
		toolRequest := &approval.ToolRequest{
			RequestID:    "provider-" + testCase.Name,
			TurnID:       "provider-conformance",
			SessionID:    testCase.Scope,
			PrincipalID:  "provider-conformance",
			ToolName:     testCase.Descriptor.Name,
			Arguments:    testCase.Arguments,
			Capabilities: []string{testCase.Descriptor.Capability},
			Trust:        approval.TrustOwnerInput,
		}
		broker, err := testCase.BrokerFactory(func(handlerCtx context.Context, _ approval.ToolRequest, _ approval.Constraints) (approval.ToolResult, error) {
			output, invokeErr := InvokeProvider(handlerCtx, testCase.Provider, &request)
			if invokeErr != nil {
				return approval.ToolResult{}, invokeErr
			}
			return approval.ToolResult{ToolName: testCase.Descriptor.Name, Output: output.Output}, nil
		})
		if err != nil {
			result.Failure = err.Error()
			results = append(results, result)
			continue
		}
		grantingBroker, ok := broker.(interface {
			approval.ToolBroker
			Grant(context.Context, *approval.ToolRequest, time.Duration) (approval.ApprovalGrant, error)
		})
		if !ok {
			result.Failure = "provider authorization boundary cannot issue execution grants"
			results = append(results, result)
			continue
		}
		grant, err := grantingBroker.Grant(ctx, toolRequest, time.Minute)
		if err == nil {
			var toolResult approval.ToolResult
			toolResult, err = grantingBroker.Execute(ctx, toolRequest, &grant)
			if err == nil {
				for _, canary := range testCase.SecretCanaries {
					if strings.Contains(string(toolResult.Output), canary) {
						result.Failure = "provider output contains a secret canary"
						break
					}
				}
			}
		}
		if err != nil {
			result.Failure = err.Error()
			results = append(results, result)
			continue
		}
		if result.Failure == "" {
			result.Passed = true
		}
		results = append(results, result)
	}
	return results
}

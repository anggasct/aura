package harness

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/approval"
)

type ProviderCase struct {
	Name           string
	Provider       Provider
	Broker         approval.ToolBroker
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
	results := make([]ProviderConformanceResult, 0, len(ordered))
	for i := range ordered {
		testCase := &ordered[i]
		result := ProviderConformanceResult{Name: testCase.Name}
		request := ProviderRequest{Descriptor: testCase.Descriptor, Arguments: testCase.Arguments, Scope: testCase.Scope}
		if testCase.Broker == nil {
			result.Failure = "provider authorization boundary is missing"
			results = append(results, result)
			continue
		}
		decision, err := testCase.Broker.Evaluate(ctx, &approval.ToolRequest{
			RequestID:    "provider-" + testCase.Name,
			TurnID:       "provider-conformance",
			SessionID:    testCase.Scope,
			PrincipalID:  "provider-conformance",
			ToolName:     testCase.Descriptor.Name,
			Arguments:    testCase.Arguments,
			Capabilities: []string{testCase.Descriptor.Capability},
			Trust:        approval.TrustOwnerInput,
		})
		if err != nil || decision.Outcome != approval.OutcomeAllow {
			if err != nil {
				result.Failure = err.Error()
			} else {
				result.Failure = "provider authorization denied"
			}
			results = append(results, result)
			continue
		}
		output, err := InvokeProvider(ctx, testCase.Provider, &request)
		if err != nil {
			result.Failure = err.Error()
			results = append(results, result)
			continue
		}
		for _, canary := range testCase.SecretCanaries {
			if strings.Contains(string(output.Output), canary) {
				result.Failure = "provider output contains a secret canary"
				break
			}
		}
		if result.Failure == "" {
			result.Passed = true
		}
		results = append(results, result)
	}
	return results
}

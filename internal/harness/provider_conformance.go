package harness

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
)

type ProviderCase struct {
	Name           string
	Provider       Provider
	Descriptor     Descriptor
	Arguments      json.RawMessage
	Scope          string
	Authorize      func(context.Context, ProviderRequest) error
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
		if testCase.Authorize != nil {
			if err := testCase.Authorize(ctx, request); err != nil {
				result.Failure = err.Error()
				results = append(results, result)
				continue
			}
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

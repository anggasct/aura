package model

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
)

var routeNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

type CapabilityPredicate struct {
	Streaming        bool
	Tools            bool
	StructuredOutput bool
	Vision           bool
	Audio            bool
	Reasoning        bool
	MinContextTokens int
	Tokenizer        string
	UsageReporting   bool
}

func (p CapabilityPredicate) SatisfiedBy(caps config.ModelCapabilities) (satisfied bool, missingField string) {
	if p.Streaming && !caps.Streaming {
		return false, "streaming"
	}
	if p.Tools && !caps.Tools {
		return false, "tools"
	}
	if p.StructuredOutput && !caps.StructuredOutput {
		return false, "structured_output"
	}
	if p.Vision && !caps.Vision {
		return false, "vision"
	}
	if p.Audio && !caps.Audio {
		return false, "audio"
	}
	if p.Reasoning && !caps.Reasoning {
		return false, "reasoning"
	}
	if p.MinContextTokens > 0 && caps.ContextTokens < p.MinContextTokens {
		return false, "context_tokens"
	}
	if p.Tokenizer != "" && caps.Tokenizer != p.Tokenizer {
		return false, "tokenizer"
	}
	if p.UsageReporting && !caps.UsageReporting {
		return false, "usage_reporting"
	}
	return true, ""
}

func PredicateForTask(task string) CapabilityPredicate {
	switch task {
	case "vision":
		return CapabilityPredicate{Vision: true}
	default:
		return CapabilityPredicate{}
	}
}

func PredicateForRequest(req *adkmodel.LLMRequest) CapabilityPredicate {
	if req == nil {
		return CapabilityPredicate{}
	}
	var pred CapabilityPredicate
	for _, t := range req.Tools {
		if t != nil {
			pred.Tools = true
			break
		}
	}
	for _, content := range req.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			if part.FunctionResponse != nil || part.FunctionCall != nil {
				pred.Tools = true
			}
			if part.InlineData != nil {
				mime := strings.ToLower(part.InlineData.MIMEType)
				if strings.HasPrefix(mime, "image/") {
					pred.Vision = true
				} else if strings.HasPrefix(mime, "audio/") {
					pred.Audio = true
				}
			}
		}
	}
	if req.Config != nil {
		if len(req.Config.Tools) > 0 {
			pred.Tools = true
		}
		if req.Config.ResponseSchema != nil || req.Config.ResponseJsonSchema != nil || strings.EqualFold(req.Config.ResponseMIMEType, "application/json") {
			pred.StructuredOutput = true
		}
		if req.Config.ThinkingConfig != nil {
			pred.Reasoning = true
		}
		if req.Config.SpeechConfig != nil {
			pred.Audio = true
		}
	}
	return pred
}

type Route struct {
	Name                string
	Candidates          []string
	MaxProviderAttempts int
	RetryDelayBudget    time.Duration
	CostBudgetUSD       float64
	Circuit             config.ModelRouteCircuit
}

func ValidateRoute(name string, route config.ModelRoute, definitions map[string]config.ModelDefinition, predicate *CapabilityPredicate) error {
	if !routeNamePattern.MatchString(name) {
		return newError(ErrorCodeRouteInvalid, name, "", fmt.Sprintf("invalid model route name %q", name))
	}
	if len(route.Candidates) < 1 || len(route.Candidates) > 4 {
		return newError(ErrorCodeRouteInvalid, name, "", "route chain depth must be between 1 and 4 candidates")
	}
	if route.MaxProviderAttempts < 0 {
		return newError(ErrorCodeRouteInvalid, name, "", "max_provider_attempts must not be negative")
	}
	if route.RetryDelayBudget < 0 {
		return newError(ErrorCodeRouteInvalid, name, "", "retry_delay_budget must not be negative")
	}
	if route.CostBudgetUSD < 0 {
		return newError(ErrorCodeRouteInvalid, name, "", "cost_budget_usd must not be negative")
	}
	seen := make(map[string]bool, len(route.Candidates))
	for _, candidate := range route.Candidates {
		if strings.Contains(candidate, "://") || strings.Contains(candidate, "/") {
			return newError(ErrorCodeRouteInvalid, name, "", fmt.Sprintf("route %q candidate %q must not contain inline endpoints", name, candidate))
		}
		if strings.HasPrefix(candidate, "sk-") || strings.HasPrefix(candidate, "bearer") {
			return newError(ErrorCodeRouteInvalid, name, "", fmt.Sprintf("route %q candidate %q must not contain inline secrets", name, candidate))
		}
		if !routeNamePattern.MatchString(candidate) {
			return newError(ErrorCodeRouteInvalid, name, "", fmt.Sprintf("invalid candidate name %q in route %q", candidate, name))
		}
		if seen[candidate] {
			return newError(ErrorCodeRouteInvalid, name, "", fmt.Sprintf("route %q contains duplicate candidate %q", name, candidate))
		}
		seen[candidate] = true

		def, ok := definitions[candidate]
		if !ok {
			return newError(ErrorCodeNotFound, candidate, "", fmt.Sprintf("route %q references unknown model definition %q", name, candidate))
		}
		if err := ValidateCandidate(candidate, &def, predicate); err != nil {
			return err
		}
	}
	return nil
}

func ValidateCandidate(candidate string, def *config.ModelDefinition, predicate *CapabilityPredicate) error {
	if def == nil || def.Capabilities.ContextTokens <= 0 || strings.TrimSpace(def.Capabilities.Tokenizer) == "" {
		return newError(ErrorCodeCapabilityUnsupported, candidate, "", fmt.Sprintf("candidate %q has invalid or missing capability metadata", candidate))
	}
	if predicate != nil {
		if ok, missing := predicate.SatisfiedBy(def.Capabilities); !ok {
			return newError(ErrorCodeCapabilityUnsupported, candidate, missing, fmt.Sprintf("candidate %q cannot satisfy required capability %q", candidate, missing))
		}
	}
	return nil
}

func ValidateRoutes(definitions map[string]config.ModelDefinition, routes map[string]config.ModelRoute, routing map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		route := routes[name]
		if err := ValidateRoute(name, route, definitions, nil); err != nil {
			return err
		}
	}
	for task, role := range routing {
		pred := PredicateForTask(task)
		route, ok := routes[role]
		if !ok {
			if def, defOk := definitions[role]; defOk {
				if ok, missing := pred.SatisfiedBy(def.Capabilities); !ok {
					return newError(ErrorCodeCapabilityUnsupported, role, missing, fmt.Sprintf("task %q requires capability %q", task, missing))
				}
				continue
			}
			continue
		}
		for _, candidate := range route.Candidates {
			def, ok := definitions[candidate]
			if !ok {
				return newError(ErrorCodeNotFound, candidate, "", fmt.Sprintf("unknown candidate %q in route %q", candidate, role))
			}
			if ok, missing := pred.SatisfiedBy(def.Capabilities); !ok {
				return newError(ErrorCodeCapabilityUnsupported, candidate, missing, fmt.Sprintf("task %q requires capability %q", task, missing))
			}
		}
	}
	return nil
}

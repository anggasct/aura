package model

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sync"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/usage"
)

var registeredModelPatterns = struct {
	sync.Mutex
	patterns map[string]bool
}{patterns: map[string]bool{}}

// RegisterAdapters registers the configured primary and auxiliary model names
// with the model registry so NewLLM can resolve them. Call once per process:
// overlapping patterns break NewLLM's exactly-one-match rule, so a duplicate
// registration of the same model name is rejected. Every adapter is validated
// before any is registered, so a failure never leaves half-registered state.
func RegisterAdapters(logger *slog.Logger, models config.Models) error {
	return RegisterAdaptersWithRoutes(context.Background(), logger, models, nil, nil, nil)
}

// RegisterAdaptersWithRoutes is the production registration path: model
// definitions land in the registry as before, and each configured model
// route additionally registers its route name onto the route's
// FallbackAdapter, so resolving a configured route name dispatches through
// fallback, circuit, and budget handling instead of the first candidate
// only. The circuit manager loads persisted checkpoints when checkpoint is
// non-nil, so an open circuit survives process restarts. Route cost budgets
// are enforced through prices when it is non-nil. Call once per process; a
// rejected registration never leaves half-registered state.
func RegisterAdaptersWithRoutes(ctx context.Context, logger *slog.Logger, models config.Models, routes map[string]config.ModelRoute, checkpoint CircuitCheckpointStore, prices *usage.PriceRegistry) error {
	adapters, circuits, err := BuildComponents(logger, models, routes, checkpoint, prices)
	if err != nil {
		return err
	}
	if circuits != nil && checkpoint != nil {
		if err := circuits.LoadCheckpoints(ctx); err != nil {
			return err
		}
	}

	routeAdapters := make(map[string]adkmodel.LLM, len(routes))
	for name, route := range routes {
		fb := NewFallbackAdapter(name, route, models.Definitions, circuits, MapAdapterResolver(adapters)).WithLogger(logger).WithPrices(prices)
		routeAdapters[name] = fb
	}

	timeout := time.Duration(models.RequestTimeout)
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	idleTimeout := time.Duration(models.StreamingIdleTimeout)
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamingIdleTimeout
	}

	type registration struct {
		role    string
		pattern string
		spec    config.ModelDefinition
		factory func() (adkmodel.LLM, error)
	}
	registrations := make([]registration, 0, len(models.Definitions)+len(routeAdapters))
	for _, role := range slices.Sorted(maps.Keys(models.Definitions)) {
		spec := models.Definitions[role]
		_, configured, err := newAdapter(logger, role, &spec, timeout, idleTimeout)
		if err != nil {
			return err
		}
		if !configured {
			continue
		}
		def := spec
		definitionID := role
		registrations = append(registrations, registration{
			role:    role,
			pattern: "^" + regexp.QuoteMeta(spec.Model) + "$",
			spec:    spec,
			factory: func() (adkmodel.LLM, error) {
				adapter, _, err := newAdapter(logger, definitionID, &def, timeout, idleTimeout)
				return adapter, err
			},
		})
	}
	for _, name := range slices.Sorted(maps.Keys(routeAdapters)) {
		definition, ok := models.Definitions[name]
		if !ok {
			definition = config.ModelDefinition{}
		}
		adapter := routeAdapters[name]
		routeName := name
		registrations = append(registrations, registration{
			role:    routeName,
			pattern: "^" + regexp.QuoteMeta(routeName) + "$",
			spec:    definition,
			factory: func() (adkmodel.LLM, error) {
				return adapter, nil
			},
		})
	}

	registeredModelPatterns.Lock()
	defer registeredModelPatterns.Unlock()
	seen := make(map[string]bool, len(registrations))
	for i := range registrations {
		reg := &registrations[i]
		if registeredModelPatterns.patterns[reg.pattern] || seen[reg.pattern] {
			return newError(ErrorCodeProtocolInvalid, reg.role, "", fmt.Sprintf("model %q is already registered", reg.spec.Model))
		}
		seen[reg.pattern] = true
	}
	for i := range registrations {
		reg := &registrations[i]
		registeredModelPatterns.patterns[reg.pattern] = true
		factory := reg.factory
		adkmodel.Register(reg.pattern, func(_ context.Context, _ string) (adkmodel.LLM, error) {
			return factory()
		})
	}
	return nil
}

// RouteModelName maps an agent definition's model route onto the model name
// the ADK registry can resolve: a configured model route resolves to its own
// route name (registered onto the route's FallbackAdapter), and an unset
// route falls back to the runtime default model when that route is
// configured, so the default path keeps fallback, circuit, and budget
// handling too.
func RouteModelName(cfg *config.Config, defaultModel string) (func(route string) (string, error), error) {
	resolver := func(route string) (string, error) {
		if r, ok := cfg.ModelRoutes[route]; ok && len(r.Candidates) > 0 {
			return route, nil
		}
		if route == "" {
			if cfg.Models.Definitions["primary"].Model == "" {
				return "", errors.New("no default model is configured")
			}
			return defaultModel, nil
		}
		if definition, ok := cfg.Models.Definitions[route]; ok && definition.Model != "" {
			return definition.Model, nil
		}
		return "", fmt.Errorf("unknown model route %q", route)
	}
	if defaultModel == "" {
		defaultModel = cfg.Models.Definitions["primary"].Model
		if defaultModel == "" {
			return nil, errors.New("model: no primary model is configured")
		}
	}
	return resolver, nil
}

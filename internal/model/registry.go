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

func RegisterAdapters(logger *slog.Logger, models config.Models) error {
	return RegisterAdaptersWithRoutes(context.Background(), logger, models, nil, nil, nil)
}

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

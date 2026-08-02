package model

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
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
	}
	registrations := make([]registration, 0, 2)
	for _, role := range []string{"primary", "auxiliary"} {
		spec := models.Definitions[role]
		_, configured, err := newAdapter(logger, role, &spec, timeout, idleTimeout)
		if err != nil {
			return err
		}
		if !configured {
			continue
		}
		registrations = append(registrations, registration{
			role:    role,
			pattern: "^" + regexp.QuoteMeta(spec.Model) + "$",
			spec:    spec,
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
		spec := reg.spec
		adkmodel.Register(reg.pattern, func(_ context.Context, _ string) (adkmodel.LLM, error) {
			adapter, _, err := newAdapter(logger, reg.role, &spec, timeout, idleTimeout)
			return adapter, err
		})
	}
	return nil
}

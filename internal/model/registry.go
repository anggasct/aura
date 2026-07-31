package model

import (
	"context"
	"fmt"
	"regexp"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
)

var registeredModelPatterns = map[string]bool{}

// RegisterAdapters registers the configured primary and auxiliary model names
// with the model registry so NewLLM can resolve them. Call once per process:
// overlapping patterns break NewLLM's exactly-one-match rule, so a duplicate
// registration of the same model name is rejected.
func RegisterAdapters(models config.Models) error {
	timeout := time.Duration(models.RequestTimeout)
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	idleTimeout := time.Duration(models.StreamingIdleTimeout)
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamingIdleTimeout
	}
	for _, role := range []string{"primary", "auxiliary"} {
		spec := models.Definitions[role]
		if spec.Protocol == "" || spec.Model == "" {
			continue
		}
		if _, err := newAdapter(role, spec, timeout, idleTimeout); err != nil {
			return err
		}
		pattern := "^" + regexp.QuoteMeta(spec.Model) + "$"
		if registeredModelPatterns[pattern] {
			return fmt.Errorf("model: %q already registered", spec.Model)
		}
		registeredModelPatterns[pattern] = true
		registeredRole := role
		registeredSpec := spec
		adkmodel.Register(pattern, func(ctx context.Context, name string) (adkmodel.LLM, error) {
			return newAdapter(registeredRole, registeredSpec, timeout, idleTimeout)
		})
	}
	return nil
}

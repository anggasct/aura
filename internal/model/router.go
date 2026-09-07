package model

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/usage"
)

func withoutPath(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

var knownTasks = map[string]bool{
	"agent":       true,
	"summarize":   true,
	"vision":      true,
	"title":       true,
	"curation":    true,
	"compression": true,
	"profiling":   true,
}

var defaultTaskRouting = map[string]string{
	"agent":       "primary",
	"summarize":   "auxiliary",
	"title":       "auxiliary",
	"curation":    "auxiliary",
	"compression": "auxiliary",
	"profiling":   "auxiliary",
}

const (
	defaultRequestTimeout       = 120 * time.Second
	defaultStreamingIdleTimeout = 60 * time.Second
)

type Router struct {
	primary     adkmodel.LLM
	auxiliary   adkmodel.LLM
	routing     map[string]string
	routes      map[string]adkmodel.LLM
	circuits    *CircuitManager
	definitions map[string]config.ModelDefinition
	components  struct {
		adapters map[string]adkmodel.LLM
		routes   map[string]adkmodel.LLM
	}
}

func NewRouter(primary, auxiliary adkmodel.LLM, routing map[string]string) (*Router, error) {
	r := &Router{primary: primary, auxiliary: auxiliary, routing: map[string]string{}}
	for task, role := range defaultTaskRouting {
		r.routing[task] = role
	}
	for task, role := range routing {
		if !knownTasks[task] {
			return nil, newError(ErrorCodeNotFound, "", "", fmt.Sprintf("model: unknown task %q in models.routing", task))
		}
		if role != "primary" && role != "auxiliary" {
			return nil, newError(ErrorCodeProtocolInvalid, "", "", fmt.Sprintf("model: invalid role %q for task %q in models.routing (must be primary or auxiliary)", role, task))
		}
		r.routing[task] = role
	}
	return r, nil
}

func BuildRouter(logger *slog.Logger, models config.Models) (*Router, error) {
	return BuildRouterWithRoutes(logger, models, nil, nil)
}

func BuildRouterWithConfig(logger *slog.Logger, cfg *config.Config, store CircuitCheckpointStore) (*Router, error) {
	if cfg == nil {
		return BuildRouter(logger, config.Models{})
	}
	return BuildRouterWithRoutes(logger, cfg.Models, cfg.ModelRoutes, store)
}

func BuildComponents(logger *slog.Logger, models config.Models, routes map[string]config.ModelRoute, checkpoint CircuitCheckpointStore, prices *usage.PriceRegistry) (adapters map[string]adkmodel.LLM, circuits *CircuitManager, err error) {
	if err := validateRoutingCapabilities(models); err != nil {
		return nil, nil, err
	}
	timeout := time.Duration(models.RequestTimeout)
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	idleTimeout := time.Duration(models.StreamingIdleTimeout)
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamingIdleTimeout
	}

	adapters = make(map[string]adkmodel.LLM, len(models.Definitions))
	for name := range models.Definitions {
		spec := models.Definitions[name]
		adapter, configured, err := newAdapter(logger, name, &spec, timeout, idleTimeout)
		if err != nil {
			return nil, nil, err
		}
		if configured {
			adapters[name] = adapter
		}
	}

	circuits = NewCircuitManager(time.Now, checkpoint).WithLogger(logger)
	for name := range models.Definitions {
		spec := models.Definitions[name]
		digest := ComputeConfigDigest(&spec)
		policy := DefaultCircuitPolicy()
		for _, r := range routes {
			for _, c := range r.Candidates {
				if c == name && r.Circuit.FailureThreshold > 0 {
					policy = CircuitPolicy{
						FailureThreshold: r.Circuit.FailureThreshold,
						OpenDuration:     time.Duration(r.Circuit.OpenDuration),
						MaxOpenDuration:  time.Duration(r.Circuit.MaxOpenDuration),
					}
				}
			}
		}
		circuits.Register(name, spec.BaseURL, digest, policy)
	}

	for name, route := range routes {
		if err := ValidateRoute(name, route, models.Definitions, nil); err != nil {
			return nil, nil, err
		}
	}
	return adapters, circuits, nil
}

func BuildRouterWithRoutes(logger *slog.Logger, models config.Models, routes map[string]config.ModelRoute, store CircuitCheckpointStore) (*Router, error) {
	adapters, circuits, err := BuildComponents(logger, models, routes, store, nil)
	if err != nil {
		return nil, err
	}

	routeAdapters := make(map[string]adkmodel.LLM, len(routes))
	for name, route := range routes {
		fb := NewFallbackAdapter(name, route, models.Definitions, circuits, MapAdapterResolver(adapters)).WithLogger(logger)
		routeAdapters[name] = fb
	}

	primary := routeAdapters["primary"]
	if primary == nil {
		primary = adapters["primary"]
	}
	auxiliary := routeAdapters["auxiliary"]
	if auxiliary == nil {
		auxiliary = adapters["auxiliary"]
	}

	r := &Router{
		primary:     primary,
		auxiliary:   auxiliary,
		routing:     make(map[string]string),
		routes:      routeAdapters,
		circuits:    circuits,
		definitions: models.Definitions,
	}
	r.components.adapters = adapters
	r.components.routes = routeAdapters
	for task, role := range defaultTaskRouting {
		r.routing[task] = role
	}
	for task, role := range models.Routing {
		if !knownTasks[task] {
			return nil, newError(ErrorCodeNotFound, "", "", fmt.Sprintf("model: unknown task %q in models.routing", task))
		}
		if role != "primary" && role != "auxiliary" {
			return nil, newError(ErrorCodeProtocolInvalid, "", "", fmt.Sprintf("model: invalid role %q for task %q in models.routing (must be primary or auxiliary)", role, task))
		}
		r.routing[task] = role
	}
	return r, nil
}

func BuildRouterComponents(r *Router) (adapters, routes map[string]adkmodel.LLM) {
	if r == nil {
		return nil, nil
	}
	return r.components.adapters, r.components.routes
}

func validateRoutingCapabilities(models config.Models) error {
	routing := make(map[string]string, len(defaultTaskRouting)+len(models.Routing))
	for task, role := range defaultTaskRouting {
		routing[task] = role
	}
	for task, role := range models.Routing {
		if knownTasks[task] {
			routing[task] = role
		}
	}
	for task, capability := range map[string]string{"vision": "vision"} {
		role := routing[task]
		if role == "" {
			continue
		}
		definition := models.Definitions[role]
		if definition.Protocol == "" {
			continue
		}
		if !modelHasCapability(definition.Capabilities, capability) {
			return newError(ErrorCodeCapabilityUnsupported, role, capability, fmt.Sprintf("task %q requires capability %q", task, capability))
		}
	}
	return nil
}

func modelHasCapability(capabilities config.ModelCapabilities, name string) bool {
	switch name {
	case "streaming":
		return capabilities.Streaming
	case "tools":
		return capabilities.Tools
	case "structured_output":
		return capabilities.StructuredOutput
	case "vision":
		return capabilities.Vision
	case "audio":
		return capabilities.Audio
	case "reasoning":
		return capabilities.Reasoning
	case "usage_reporting":
		return capabilities.UsageReporting
	default:
		return false
	}
}

func retryConfigForRequest(base RetryConfig, req *adkmodel.LLMRequest) RetryConfig {
	if requestHasToolResult(req) {
		base.MaxRetries = 0
	}
	return base
}

func requestHasToolResult(req *adkmodel.LLMRequest) bool {
	if req == nil {
		return false
	}
	for _, content := range req.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part != nil && part.FunctionResponse != nil {
				return true
			}
		}
	}
	return false
}

func (r *Router) For(task string) (adkmodel.LLM, error) {
	role, ok := r.routing[task]
	if !ok {
		if task == "" || task == "turn" {
			role = "primary"
		} else {
			return nil, newError(ErrorCodeNotFound, "", "", fmt.Sprintf("model: unknown task %q", task))
		}
	}
	if r.routes != nil {
		if adapter, exists := r.routes[role]; exists && adapter != nil {
			return adapter, nil
		}
	}
	switch role {
	case "primary":
		if r.primary == nil {
			return nil, newError(ErrorCodeNotFound, "", "", "model: no primary model is configured")
		}
		return r.primary, nil
	default:
		if r.auxiliary == nil {
			return nil, newError(ErrorCodeNotFound, "", "", "model: no auxiliary model is configured")
		}
		return r.auxiliary, nil
	}
}

func (r *Router) ForRoute(route string) (adkmodel.LLM, error) {
	if r.routes != nil {
		if adapter, exists := r.routes[route]; exists && adapter != nil {
			return adapter, nil
		}
	}
	switch route {
	case "primary":
		if r.primary != nil {
			return r.primary, nil
		}
	case "auxiliary":
		if r.auxiliary != nil {
			return r.auxiliary, nil
		}
	}
	return nil, newError(ErrorCodeNotFound, "", "", fmt.Sprintf("model: route %q not found", route))
}

func (r *Router) Circuits() *CircuitManager {
	return r.circuits
}

func (r *Router) Definitions() map[string]config.ModelDefinition {
	return r.definitions
}
func newAdapter(logger *slog.Logger, name string, spec *config.ModelDefinition, timeout, idleTimeout time.Duration) (adapter adkmodel.LLM, configured bool, err error) {
	if spec.Protocol == "" || spec.Model == "" {
		return nil, false, nil
	}
	if spec.BaseURL != "" {
		if err := config.ValidateBaseURL(spec.BaseURL); err != nil {
			return nil, false, newError(ErrorCodeProtocolInvalid, name, "", fmt.Sprintf("invalid base_url: %v", err))
		}
	}
	switch spec.Protocol {
	case config.ProtocolAnthropicMessages, config.ProtocolOpenAIChatCompat, config.ProtocolOpenAIResponses, config.ProtocolGeminiNative:
	default:
		return nil, false, newError(ErrorCodeProtocolInvalid, name, "", fmt.Sprintf("unsupported protocol %q", spec.Protocol))
	}
	apiKey, err := resolveSecret(name, spec)
	if err != nil {
		return nil, false, err
	}
	switch spec.Protocol {
	case config.ProtocolAnthropicMessages:
		return newAnthropicAdapter(logger, spec.Model, spec.BaseURL, apiKey, timeout, idleTimeout), true, nil
	case config.ProtocolOpenAIChatCompat:
		return newOpenAIAdapter(logger, spec.Model, spec.BaseURL, apiKey, timeout, idleTimeout), true, nil
	case config.ProtocolOpenAIResponses:
		return newOpenAIResponsesAdapter(logger, spec.Model, spec.BaseURL, apiKey, timeout, idleTimeout), true, nil
	case config.ProtocolGeminiNative:
		return newGeminiAdapter(logger, spec.Model, spec.BaseURL, apiKey, timeout, idleTimeout), true, nil
	}
	return nil, false, newError(ErrorCodeProtocolInvalid, name, "", fmt.Sprintf("unsupported protocol %q", spec.Protocol))
}

func resolveSecret(name string, spec *config.ModelDefinition) (string, error) {
	if spec.APIKeyEnv != "" && spec.APIKeyFile != "" {
		return "", newError(ErrorCodeSecretInvalid, name, "", "set only one of api_key_env or api_key_file")
	}
	if spec.APIKeyEnv != "" {
		value, ok := os.LookupEnv(spec.APIKeyEnv)
		if !ok || (value == "" && !config.IsLoopbackBaseURL(spec.BaseURL)) {
			return "", newError(ErrorCodeSecretInvalid, name, "", fmt.Sprintf("secret environment variable %q is unavailable", spec.APIKeyEnv))
		}
		return value, nil
	}
	if spec.APIKeyFile != "" {
		data, err := os.ReadFile(spec.APIKeyFile)
		if err != nil {
			return "", newError(ErrorCodeSecretInvalid, name, "", fmt.Sprintf("cannot read secret file %q: %v", filepath.Base(spec.APIKeyFile), withoutPath(err)))
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			return "", newError(ErrorCodeSecretInvalid, name, "", "secret file is empty")
		}
		return value, nil
	}
	if !config.IsLoopbackBaseURL(spec.BaseURL) {
		return "", newError(ErrorCodeSecretInvalid, name, "", "api_key_env or api_key_file is required for a non-loopback endpoint")
	}
	return "", nil
}

func rejectCrossOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	previous := via[len(via)-1].URL
	if redirectOrigin(previous) != redirectOrigin(req.URL) {
		return errors.New("cross-origin redirect rejected")
	}
	return nil
}

func redirectOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return u.Scheme + "://" + u.Hostname() + ":" + port
}

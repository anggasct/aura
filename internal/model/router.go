package model

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
)

// withoutPath strips the filesystem path from an *fs.PathError so an error
// string keeps the reason without disclosing where the file lives.
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
	primary   adkmodel.LLM
	auxiliary adkmodel.LLM
	routing   map[string]string
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

func BuildRouter(models config.Models) (*Router, error) {
	if err := validateRoutingCapabilities(models); err != nil {
		return nil, err
	}
	timeout := time.Duration(models.RequestTimeout)
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	idleTimeout := time.Duration(models.StreamingIdleTimeout)
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamingIdleTimeout
	}
	primarySpec := models.Definitions["primary"]
	primary, _, err := newAdapter("primary", &primarySpec, timeout, idleTimeout)
	if err != nil {
		return nil, err
	}
	auxiliarySpec := models.Definitions["auxiliary"]
	auxiliary, _, err := newAdapter("auxiliary", &auxiliarySpec, timeout, idleTimeout)
	if err != nil {
		return nil, err
	}
	return NewRouter(primary, auxiliary, models.Routing)
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

// For resolves the model for a task. An unknown task or a role with no
// configured model is a typed error; nil is never returned as a usable
// adapter.
func (r *Router) For(task string) (adkmodel.LLM, error) {
	role, ok := r.routing[task]
	if !ok {
		if task == "" || task == "turn" {
			role = "primary"
		} else {
			return nil, newError(ErrorCodeNotFound, "", "", fmt.Sprintf("model: unknown task %q", task))
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

// newAdapter reports configured=false when the definition is absent, so a
// caller never has to read meaning into a nil adapter with a nil error.
func newAdapter(name string, spec *config.ModelDefinition, timeout, idleTimeout time.Duration) (adapter adkmodel.LLM, configured bool, err error) {
	if spec.Protocol == "" || spec.Model == "" {
		return nil, false, nil
	}
	if spec.BaseURL != "" {
		// The URL is never echoed back: it may carry user-info credentials.
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
		return newAnthropicAdapter(spec.Model, spec.BaseURL, apiKey, timeout, idleTimeout), true, nil
	case config.ProtocolOpenAIChatCompat:
		return newOpenAIAdapter(spec.Model, spec.BaseURL, apiKey, timeout, idleTimeout), true, nil
	case config.ProtocolOpenAIResponses:
		return newOpenAIResponsesAdapter(spec.Model, spec.BaseURL, apiKey, timeout, idleTimeout), true, nil
	case config.ProtocolGeminiNative:
		return newGeminiAdapter(spec.Model, spec.BaseURL, apiKey, timeout, idleTimeout), true, nil
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

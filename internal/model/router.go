package model

import (
	"fmt"
	"net/url"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/config"
)

var knownTasks = map[string]bool{
	"summarize":        true,
	"vision":           true,
	"title_gen":        true,
	"curator":          true,
	"context_compress": true,
	"profiling":        true,
}

var defaultTaskRouting = map[string]string{
	"summarize":        "auxiliary",
	"vision":           "primary",
	"title_gen":        "auxiliary",
	"curator":          "auxiliary",
	"context_compress": "auxiliary",
	"profiling":        "auxiliary",
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
			return nil, fmt.Errorf("model: unknown task %q in models.routing (known: summarize, vision, title_gen, curator, context_compress, profiling)", task)
		}
		if role != "primary" && role != "auxiliary" {
			return nil, fmt.Errorf("model: invalid role %q for task %q in models.routing (must be primary or auxiliary)", role, task)
		}
		r.routing[task] = role
	}
	return r, nil
}

func BuildRouter(models config.Models) (*Router, error) {
	timeout := time.Duration(models.RequestTimeout)
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	idleTimeout := time.Duration(models.StreamingIdleTimeout)
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamingIdleTimeout
	}
	primary, err := newAdapter(models.Primary, timeout, idleTimeout)
	if err != nil {
		return nil, err
	}
	auxiliary, err := newAdapter(models.Auxiliary, timeout, idleTimeout)
	if err != nil {
		return nil, err
	}
	return NewRouter(primary, auxiliary, models.Routing)
}

func (r *Router) For(task string) adkmodel.LLM {
	if task == "" || task == "turn" {
		return r.primary
	}
	if r.routing[task] == "primary" {
		return r.primary
	}
	return r.auxiliary
}

func newAdapter(spec config.ModelSpec, timeout, idleTimeout time.Duration) (adkmodel.LLM, error) {
	if spec.Provider == "" {
		return nil, nil
	}
	if spec.BaseURL != "" {
		if err := validateBaseURL(spec.BaseURL); err != nil {
			return nil, fmt.Errorf("model: invalid base_url %q for provider %q: %w", spec.BaseURL, spec.Provider, err)
		}
	}
	if spec.APIKey == "" && !isLocalhost(spec.BaseURL) {
		return nil, fmt.Errorf("model: missing api_key for provider %q (set api_key, or use a localhost base_url)", spec.Provider)
	}
	switch spec.Provider {
	case "anthropic":
		return newAnthropicAdapter(spec.Model, spec.BaseURL, spec.APIKey, timeout, idleTimeout), nil
	case "openai", "openrouter", "ollama", "vllm", "gemini", "lmstudio":
		return newOpenAIAdapter(spec.Model, spec.BaseURL, spec.APIKey, timeout, idleTimeout), nil
	default:
		return nil, fmt.Errorf("model: unsupported provider %q", spec.Provider)
	}
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("missing host")
	}
	if u.User != nil {
		return fmt.Errorf("user info is not allowed")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("query and fragment are not allowed")
	}
	if u.Scheme == "http" && !isLocalhost(raw) {
		return fmt.Errorf("https is required for non-localhost endpoints")
	}
	return nil
}

func isLocalhost(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

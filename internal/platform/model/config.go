package model

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	ProtocolOpenAIResponses   = "openai_responses"
	ProtocolOpenAIChatCompat  = "openai_chat_compat"
	ProtocolAnthropicMessages = "anthropic_messages"
	ProtocolGeminiNative      = "gemini_native"
)

const (
	ProviderAuthBearer      = "bearer"
	ProviderSessionNone     = "none"
	ProviderSessionOpencode = "opencode-session"
	SessionHeaderName       = "x-opencode-session"
)

const (
	DefaultModelRouteMaxProviderAttempts = 4
	DefaultModelRouteRetryDelayBudget    = 20 * time.Second
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

type Config struct {
	Definitions          map[string]Definition     `json:"definitions"`
	Providers            map[string]ProviderPreset `json:"providers,omitempty"`
	RequestTimeout       configkit.Duration        `json:"request_timeout"`
	StreamingIdleTimeout configkit.Duration        `json:"streaming_idle_timeout"`
	Routing              map[string]string         `json:"routing"`
}

type Definition struct {
	Protocol     string          `json:"protocol,omitempty"`
	Model        string          `json:"model"`
	BaseURL      string          `json:"base_url,omitempty"`
	Provider     string          `json:"provider,omitempty"`
	Custom       *CustomProvider `json:"custom,omitempty"`
	APIKeyRef    secret.Ref      `json:"api_key_ref"`
	Capabilities Capabilities    `json:"capabilities"`
}

type Capabilities struct {
	Streaming            bool   `json:"streaming"`
	Tools                bool   `json:"tools"`
	StructuredOutput     bool   `json:"structured_output"`
	Vision               bool   `json:"vision"`
	Audio                bool   `json:"audio"`
	Reasoning            bool   `json:"reasoning"`
	ContextTokens        int    `json:"context_tokens"`
	Tokenizer            string `json:"tokenizer"`
	UsageReporting       bool   `json:"usage_reporting"`
	MicrosPerInputToken  int64  `json:"micros_per_input_token"`
	MicrosPerOutputToken int64  `json:"micros_per_output_token"`
}

type ProviderPreset struct {
	Protocol        string            `json:"protocol"`
	BaseURL         string            `json:"base_url"`
	AuthScheme      string            `json:"auth_scheme"`
	Headers         map[string]string `json:"headers,omitempty"`
	SessionBehavior string            `json:"session_behavior"`
}

type CustomProvider = ProviderPreset

type ResolvedDefinition struct {
	Protocol        string
	BaseURL         string
	Headers         map[string]string
	SessionBehavior string
	ProviderName    string
}

type Routes map[string]ModelRoute

type ModelRoute struct {
	Candidates          []string           `json:"candidates"`
	MaxProviderAttempts int                `json:"max_provider_attempts"`
	RetryDelayBudget    configkit.Duration `json:"retry_delay_budget"`
	CostBudgetUSD       float64            `json:"cost_budget_usd"`
	Circuit             RouteCircuit       `json:"circuit"`
}

type RouteCircuit struct {
	FailureThreshold int                `json:"failure_threshold"`
	OpenDuration     configkit.Duration `json:"open_duration"`
	MaxOpenDuration  configkit.Duration `json:"max_open_duration"`
}

func ProtocolNames() []string {
	return []string{ProtocolOpenAIResponses, ProtocolOpenAIChatCompat, ProtocolAnthropicMessages, ProtocolGeminiNative}
}

func ValidProtocol(p string) bool {
	return slices.Contains(ProtocolNames(), p)
}

func Default() Config {
	return Config{
		Definitions:          map[string]Definition{},
		RequestTimeout:       configkit.Duration(120 * time.Second),
		StreamingIdleTimeout: configkit.Duration(60 * time.Second),
		Routing:              defaultRouting(),
	}
}

func defaultRouting() map[string]string {
	return map[string]string{
		"agent":       "primary",
		"summarize":   "auxiliary",
		"title":       "auxiliary",
		"curation":    "auxiliary",
		"compression": "auxiliary",
		"profiling":   "auxiliary",
	}
}

func DefaultModelRoute() ModelRoute {
	return ModelRoute{
		MaxProviderAttempts: DefaultModelRouteMaxProviderAttempts,
		RetryDelayBudget:    configkit.Duration(DefaultModelRouteRetryDelayBudget),
	}
}

func (*ModelRoute) Default() any {
	return DefaultModelRoute()
}

func BuiltinProviders() map[string]ProviderPreset {
	return map[string]ProviderPreset{
		"openai": {
			Protocol:        ProtocolOpenAIResponses,
			BaseURL:         "https://api.openai.com/v1",
			AuthScheme:      ProviderAuthBearer,
			SessionBehavior: ProviderSessionNone,
		},
		"anthropic": {
			Protocol:        ProtocolAnthropicMessages,
			BaseURL:         "https://api.anthropic.com",
			AuthScheme:      ProviderAuthBearer,
			SessionBehavior: ProviderSessionNone,
		},
		"gemini": {
			Protocol:        ProtocolGeminiNative,
			BaseURL:         "https://generativelanguage.googleapis.com",
			AuthScheme:      ProviderAuthBearer,
			SessionBehavior: ProviderSessionNone,
		},
		"openrouter": {
			Protocol:        ProtocolOpenAIChatCompat,
			BaseURL:         "https://openrouter.ai/api/v1",
			AuthScheme:      ProviderAuthBearer,
			SessionBehavior: ProviderSessionNone,
		},
		"opencode-go": {
			Protocol:        ProtocolOpenAIResponses,
			BaseURL:         "https://opencode.ai/zen/go",
			AuthScheme:      ProviderAuthBearer,
			SessionBehavior: ProviderSessionOpencode,
		},
	}
}

func (c Config) ResolveDefinition(def *Definition) (ResolvedDefinition, error) {
	return c.resolveDefinition(def, "models.definitions")
}

func (c Config) resolveDefinition(def *Definition, where string) (ResolvedDefinition, error) {
	registry := BuiltinProviders()
	for name, preset := range c.Providers {
		registry[name] = preset
	}
	if def.Provider != "" && def.Custom != nil {
		return ResolvedDefinition{}, configkit.Field(where, fault.New(ErrorCodeProviderInvalid, "entry sets both provider and custom"))
	}
	if def.Custom != nil {
		if strings.TrimSpace(def.Protocol) != "" || strings.TrimSpace(def.BaseURL) != "" {
			return ResolvedDefinition{}, configkit.Field(where, fault.New(ErrorCodeProviderInvalid, "entry with custom must not set protocol or base_url"))
		}
		if err := validateProviderShape(where, *def.Custom); err != nil {
			return ResolvedDefinition{}, err
		}
		return ResolvedDefinition{
			Protocol:        def.Custom.Protocol,
			BaseURL:         strings.TrimRight(def.Custom.BaseURL, "/"),
			Headers:         copyProviderHeaders(def.Custom.Headers),
			SessionBehavior: def.Custom.SessionBehavior,
			ProviderName:    "custom",
		}, nil
	}
	if def.Provider != "" {
		if strings.TrimSpace(def.Protocol) != "" {
			return ResolvedDefinition{}, configkit.Field(where, fault.New(ErrorCodeProviderInvalid, "entry with provider must not set protocol"))
		}
		preset, ok := registry[def.Provider]
		if !ok {
			return ResolvedDefinition{}, configkit.Field(where, fault.New(ErrorCodeProviderInvalid, "entry references unknown provider"))
		}
		if err := validateProviderShape(where, preset); err != nil {
			return ResolvedDefinition{}, err
		}
		baseURL := preset.BaseURL
		if strings.TrimSpace(def.BaseURL) != "" {
			baseURL = def.BaseURL
		}
		if err := egress.ValidateBaseURL(baseURL); err != nil {
			return ResolvedDefinition{}, configkit.Field(where, fault.New(ErrorCodeProviderInvalid, "entry with provider has invalid base_url: "+err.Error()))
		}
		return ResolvedDefinition{
			Protocol:        preset.Protocol,
			BaseURL:         strings.TrimRight(baseURL, "/"),
			Headers:         copyProviderHeaders(preset.Headers),
			SessionBehavior: preset.SessionBehavior,
			ProviderName:    def.Provider,
		}, nil
	}
	return ResolvedDefinition{
		Protocol:        def.Protocol,
		BaseURL:         strings.TrimRight(def.BaseURL, "/"),
		SessionBehavior: ProviderSessionNone,
	}, nil
}

func validateProviderShape(where string, preset ProviderPreset) error {
	if !ValidProtocol(preset.Protocol) {
		return configkit.Field(where, fault.New(ErrorCodeProviderInvalid, "uses unsupported protocol (must be one of "+strings.Join(ProtocolNames(), ", ")+")"))
	}
	if strings.TrimSpace(preset.BaseURL) == "" {
		return configkit.Field(where, fault.New(ErrorCodeProviderInvalid, "requires a non-empty base_url"))
	}
	if err := egress.ValidateBaseURL(preset.BaseURL); err != nil {
		return configkit.Field(where, fault.New(ErrorCodeProviderInvalid, "has invalid base_url: "+err.Error()))
	}
	if preset.AuthScheme != ProviderAuthBearer {
		return configkit.Field(where, fault.New(ErrorCodeProviderInvalid, "uses unsupported auth scheme (must be "+ProviderAuthBearer+")"))
	}
	if preset.SessionBehavior != ProviderSessionNone && preset.SessionBehavior != ProviderSessionOpencode {
		return configkit.Field(where, fault.New(ErrorCodeProviderInvalid, "uses unsupported session behavior (must be "+ProviderSessionNone+" or "+ProviderSessionOpencode+")"))
	}
	return validateProviderHeaders(where, preset.Headers)
}

func validateProviderHeaders(where string, headers map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(headers)) {
		value := headers[name]
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t:") {
			return configkit.Field(where, fault.New(ErrorCodeProviderInvalid, "sets invalid header name"))
		}
		if strings.EqualFold(name, "authorization") || strings.EqualFold(name, "content-type") || strings.EqualFold(name, SessionHeaderName) {
			return configkit.Field(where, fault.New(ErrorCodeProviderInvalid, fmt.Sprintf("header %q collides with a managed header", name)))
		}
		trimmed := strings.TrimSpace(value)
		if strings.HasPrefix(trimmed, "env://") || strings.HasPrefix(trimmed, "file://") || strings.Contains(trimmed, "${") {
			return configkit.Field(where, fault.New(ErrorCodeProviderInvalid, fmt.Sprintf("header %q must not carry a secret reference", name)))
		}
	}
	return nil
}

func copyProviderHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		out[name] = value
	}
	return out
}

func (c Config) Validate() error {
	problems := make([]error, 0, len(c.Definitions)+len(c.Providers)+1)
	for _, name := range slices.Sorted(maps.Keys(c.Providers)) {
		preset := c.Providers[name]
		if !namePattern.MatchString(name) {
			problems = append(problems, configkit.Field("providers", fault.New(ErrorCodeProviderInvalid, fmt.Sprintf("invalid name %q", name))))
			continue
		}
		if err := validateProviderShape("providers."+name, preset); err != nil {
			problems = append(problems, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(c.Definitions)) {
		if !namePattern.MatchString(name) {
			problems = append(problems, configkit.InvalidKey("definitions["+name+"]", "has an invalid name (must match "+namePattern.String()+")"))
			continue
		}
		definition := c.Definitions[name]
		problems = append(problems, c.definitionProblems(name, &definition)...)
	}
	return errors.Join(problems...)
}

func (c Config) definitionProblems(name string, definition *Definition) []error {
	var problems []error
	if definition.Provider != "" || definition.Custom != nil {
		return append(problems, c.providerDefinitionProblems(name, definition)...)
	}
	switch {
	case strings.TrimSpace(definition.Protocol) == "" || strings.TrimSpace(definition.Model) == "":
		problems = append(problems, configkit.Field("definitions."+name, fault.New(ErrorCodeProtocolInvalid, "requires non-empty protocol and model")))
	case !ValidProtocol(definition.Protocol):
		problems = append(problems, configkit.Field("definitions."+name, fault.New(ErrorCodeProtocolInvalid, "uses unsupported protocol (must be one of "+strings.Join(ProtocolNames(), ", ")+")")))
	}
	if err := egress.ValidateBaseURL(definition.BaseURL); err != nil {
		problems = append(problems, configkit.Field("definitions."+name+".base_url", fault.New(ErrorCodeProtocolInvalid, err.Error())))
	}
	problems = append(problems, secretProblems(name, definition, definition.BaseURL)...)
	problems = append(problems, capabilityProblems(name, definition)...)
	return problems
}

func (c Config) providerDefinitionProblems(name string, definition *Definition) []error {
	var problems []error
	if strings.TrimSpace(definition.Model) == "" {
		problems = append(problems, configkit.Field("definitions."+name, fault.New(ErrorCodeProviderInvalid, "requires non-empty model")))
	}
	resolved, err := c.resolveDefinition(definition, "definitions")
	if err != nil {
		problems = append(problems, err)
		return append(problems, capabilityProblems(name, definition)...)
	}
	problems = append(problems, secretProblems(name, definition, resolved.BaseURL)...)
	problems = append(problems, capabilityProblems(name, definition)...)
	return problems
}

func secretProblems(name string, definition *Definition, baseURL string) []error {
	var problems []error
	if definition.APIKeyRef.IsZero() && !egress.IsLoopbackBaseURL(baseURL) {
		problems = append(problems, configkit.Field("definitions."+name, fault.New(ErrorCodeSecretInvalid, "requires api_key_ref for a non-loopback endpoint")))
	}
	return problems
}

func capabilityProblems(name string, definition *Definition) []error {
	var problems []error
	if definition.Capabilities.ContextTokens <= 0 || strings.TrimSpace(definition.Capabilities.Tokenizer) == "" {
		problems = append(problems, configkit.Field("definitions."+name, fault.New(ErrorCodeCapabilityUnsupported, "requires positive context_tokens and a tokenizer")))
	}
	if definition.Capabilities.MicrosPerInputToken < 0 || definition.Capabilities.MicrosPerOutputToken < 0 {
		problems = append(problems, configkit.Field("definitions."+name+".capabilities", fault.New(ErrorCodeCapabilityUnsupported, "cost rates must not be negative")))
	}
	return problems
}

func ValidateRoutes(routes Routes) error {
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		route := routes[name]
		if !namePattern.MatchString(name) {
			problems = append(problems, configkit.Invalid("", fmt.Sprintf("invalid route name %q", name)))
		}
		if len(route.Candidates) < 1 {
			problems = append(problems, configkit.Invalid(name, "requires at least one candidate"))
		} else if len(route.Candidates) > 4 {
			problems = append(problems, configkit.Invalid(name, "exceeds maximum chain depth of 4 candidates"))
		}
		seen := make(map[string]bool, len(route.Candidates))
		for _, candidate := range route.Candidates {
			if seen[candidate] {
				problems = append(problems, configkit.Invalid(name, "contains a duplicate candidate"))
			}
			seen[candidate] = true
		}
		if route.MaxProviderAttempts < 0 {
			problems = append(problems, configkit.Invalid(name+".max_provider_attempts", "must not be negative"))
		}
		if route.RetryDelayBudget < 0 {
			problems = append(problems, configkit.Invalid(name+".retry_delay_budget", "must not be negative"))
		}
		if route.CostBudgetUSD < 0 {
			problems = append(problems, configkit.Invalid(name+".cost_budget_usd", "must not be negative"))
		}
		if route.Circuit.FailureThreshold < 0 {
			problems = append(problems, configkit.Invalid(name+".circuit.failure_threshold", "must not be negative"))
		}
		if route.Circuit.OpenDuration < 0 {
			problems = append(problems, configkit.Invalid(name+".circuit.open_duration", "must not be negative"))
		}
		if route.Circuit.MaxOpenDuration < 0 {
			problems = append(problems, configkit.Invalid(name+".circuit.max_open_duration", "must not be negative"))
		}
	}
	return errors.Join(problems...)
}

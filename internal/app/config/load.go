package config

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/go-viper/mapstructure/v2"

	legacyconfig "github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	envPrefix        = "AURA_"
	envConfigVar     = "AURA_CONFIG"
	supportedVersion = 1

	githubCapability = "provider-github"
)

var (
	modelDefinitionNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
)

type LoadResult struct {
	Config               *Config
	Path                 string
	DefaultGenerated     bool
	CapabilityReport     capability.Report
	CapabilityStateError error
	Warnings             []string
}

type LoadOptions struct {
	Build        capability.Build
	Registry     capability.Registry
	Dependencies capability.Dependencies
}

func Load(path string) (LoadResult, error) {
	build, err := capability.CurrentBuild()
	if err != nil {
		return LoadResult{}, fmt.Errorf("config: %w", err)
	}
	registry, err := capability.BuiltinRegistry()
	if err != nil {
		return LoadResult{}, fmt.Errorf("config: %w", err)
	}
	return LoadWithOptions(path, LoadOptions{Build: build, Registry: registry, Dependencies: interpreter.HostDependencies(sandbox.CapabilityDependencies())})
}

func LoadWithOptions(path string, options LoadOptions) (LoadResult, error) {
	res, err := load(path, options)
	if err != nil {
		return LoadResult{}, fmt.Errorf("config: %w", err)
	}
	return res, nil
}

func load(path string, options LoadOptions) (LoadResult, error) {
	resolved, explicit, err := resolvePath(path)
	if err != nil {
		return LoadResult{}, err
	}
	data, generated, err := loadBytes(resolved, explicit)
	if err != nil {
		return LoadResult{}, err
	}
	doc, err := parseDocument(data)
	if err != nil {
		return LoadResult{}, err
	}
	version, err := doc.version()
	if err != nil {
		return LoadResult{}, err
	}
	if err := checkVersion(version); err != nil {
		return LoadResult{}, positionProblems(doc, err)
	}
	cfg, err := decode(doc)
	if err != nil {
		return LoadResult{}, err
	}
	if err := checkVersion(cfg.Version); err != nil {
		return LoadResult{}, positionProblems(doc, err)
	}
	deriveDefaults(cfg)
	if err := validateLoaded(cfg, doc, options.Build.Profile()); err != nil {
		return LoadResult{}, positionProblems(doc, err)
	}
	if err := loadPersonaSouls(cfg, resolved); err != nil {
		return LoadResult{}, positionProblems(doc, err)
	}
	report, resolveErr := options.Registry.Resolve(options.Build, cfg.Capabilities.Enabled, options.Dependencies)
	if resolveErr != nil && !capability.IsHealthState(resolveErr) {
		return LoadResult{}, resolveErr
	}
	if cfg.Tools != nil && len(cfg.Tools.GitHub.Credentials) > 0 && !slices.Contains(report.Active(), githubCapability) {
		return LoadResult{}, positionProblems(doc, configkit.Invalid("tools.github.credentials", "needs the "+githubCapability+" capability: compile it into the build and list it in capabilities.enabled"))
	}
	return LoadResult{
		Config:               cfg,
		Path:                 resolved,
		DefaultGenerated:     generated,
		CapabilityReport:     report,
		CapabilityStateError: resolveErr,
		Warnings:             envWarnings(cfg),
	}, nil
}

func validateLoaded(cfg *Config, doc *document, profile capability.Profile) error {
	return errors.Join(
		legacyconfig.ValidateCapabilities(cfg.Capabilities),
		validateLoadedModels(cfg.Models, doc),
		validateLoadedModelRoutes(cfg.ModelRoutes, cfg.Models),
		legacyconfig.ValidateTools(cfg.Tools, profile),
		legacyconfig.ValidateSkills(cfg.Skills, profile),
		legacyconfig.ValidateContext(cfg.Context, profile),
		legacyconfig.ValidateProfile(cfg.Profile, profile),
		legacyconfig.ValidateChildren(cfg.Children, profile),
		legacyconfig.ValidateAccess(cfg.Access),
		legacyconfig.ValidateSync(cfg.Sync),
		legacyconfig.ValidateVision(cfg.Vision, profile),
		legacyconfig.ValidateKnowledge(cfg.Knowledge),
		legacyconfig.ValidateInterpreter(&cfg.Interpreter),
		legacyconfig.ValidateRuntime(cfg.Runtime),
		legacyconfig.ValidateServer(cfg.Server),
		legacyconfig.ValidateLogging(cfg.Logging),
		legacyconfig.ValidateStorage(cfg.Storage),
		legacyconfig.ValidateTelemetry(cfg.Telemetry),
		legacyconfig.ValidateUsage(cfg.Usage),
		legacyconfig.ValidateHealth(cfg.Health),
		legacyconfig.ValidateTerminal(cfg.Terminal),
		legacyconfig.ValidateWorkflows(cfg.Workflows),
		legacyconfig.ValidateMCP(cfg.MCP),
		legacyconfig.ValidatePlugins(cfg.Plugins),
		legacyconfig.ValidateWebhook(&cfg.Webhook, cfg.Health.Listen),
		legacyconfig.ValidateDiscord(&cfg.Channels.Discord),
		legacyconfig.ValidateTelegram(&cfg.Channels.Telegram, &cfg.Channels.Discord, cfg.Owner),
		legacyconfig.ValidateEmail(&cfg.Channels.Email, cfg.Owner),
		legacyconfig.ValidateHTTP(&cfg.Channels.HTTP),
		legacyconfig.ValidateBindings(cfg.Bindings, &cfg.Channels),
		legacyconfig.ValidateBroadcast(&cfg.Broadcast),
		legacyconfig.ValidateScheduler(&cfg.Scheduler),
		legacyconfig.ValidateMemory(&cfg.Memory),
		legacyconfig.ValidateOwner(cfg.Owner),
		legacyconfig.ValidateOwnerIdentities(cfg.Owner),
		screenOwnerSecrets(cfg.Owner, cfg),
		validatePersonaNames(cfg),
		validatePersonaAccess(cfg),
	)
}

func resolvePath(flagPath string) (path string, explicit bool, err error) {
	if flagPath != "" {
		return flagPath, true, nil
	}
	if fromEnv := os.Getenv(envConfigVar); fromEnv != "" {
		return fromEnv, true, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", false, fmt.Errorf("cannot resolve config directory: set HOME or XDG_CONFIG_HOME: %w", err)
	}
	return filepath.Join(base, "aura", "config.yaml"), false, nil
}

func loadBytes(path string, explicit bool) (data []byte, generated bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if explicit {
				return nil, false, fmt.Errorf("file not found: %s", path)
			}
			data, err := writeDefault(path)
			return data, true, err
		}
		if errors.Is(err, fs.ErrPermission) {
			return nil, false, fmt.Errorf("cannot read %s: permission denied", path)
		}
		return nil, false, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, false, fmt.Errorf("%s is a directory, not a file", path)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return nil, false, fmt.Errorf("cannot read %s: permission denied", path)
		}
		return nil, false, fmt.Errorf("cannot read %s: %w", path, err)
	}
	return data, false, nil
}

func writeDefault(path string) ([]byte, error) {
	defaults := Default()
	data, err := Marshal(&defaults)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal default config: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create config directory: %w", err)
	}
	// Atomic write: the config only becomes visible at its final path once
	// fully written and fsynced, so a crash cannot leave a truncated file.
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("cannot create temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("cannot write default config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("cannot fsync default config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("cannot close default config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return nil, fmt.Errorf("cannot move default config into place: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, fmt.Errorf("cannot fsync config directory: %w", err)
	}
	return data, nil
}

func syncDir(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func validateLoadedModels(models legacyconfig.Models, doc *document) error {
	problems := make([]error, 0, len(models.Definitions)+len(models.Providers)+1)
	for _, name := range slices.Sorted(maps.Keys(models.Providers)) {
		preset := models.Providers[name]
		if !modelDefinitionNamePattern.MatchString(name) {
			problems = append(problems, configkit.Field("models.providers", fault.New(legacyconfig.ErrorCodeModelProviderInvalid, fmt.Sprintf("invalid name %q", name))))
			continue
		}
		if err := legacyconfig.ValidateProviderShape("models.providers."+name, preset); err != nil {
			problems = append(problems, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(models.Definitions)) {
		if !modelDefinitionNamePattern.MatchString(name) {
			problems = append(problems, &configkit.FieldError{
				Path:     "models.definitions[" + name + "]",
				Position: doc.keyPositionOf("models.definitions[" + name + "]"),
				Err:      fault.New(configkit.ErrorCodeInvalid, "has an invalid name (must match "+modelDefinitionNamePattern.String()+")"),
			})
			continue
		}
		definition := models.Definitions[name]
		problems = append(problems, modelDefinitionProblems(models, name, &definition)...)
	}
	problems = append(problems, validateRouting(models, doc))
	return errors.Join(problems...)
}

func modelDefinitionProblems(models legacyconfig.Models, name string, definition *legacyconfig.ModelDefinition) []error {
	var problems []error
	if definition.Provider != "" || definition.Custom != nil {
		return append(problems, providerDefinitionProblems(models, name, definition)...)
	}
	switch {
	case strings.TrimSpace(definition.Protocol) == "" || strings.TrimSpace(definition.Model) == "":
		problems = append(problems, configkit.Field("models.definitions."+name, fault.New(legacyconfig.ErrorCodeModelProtocolInvalid, "requires non-empty protocol and model")))
	case !legacyconfig.ValidProtocol(definition.Protocol):
		problems = append(problems, configkit.Field("models.definitions."+name, fault.New(legacyconfig.ErrorCodeModelProtocolInvalid, "uses unsupported protocol (must be one of "+strings.Join(legacyconfig.ProtocolNames(), ", ")+")")))
	}
	if err := legacyconfig.ValidateBaseURL(definition.BaseURL); err != nil {
		problems = append(problems, configkit.Field("models.definitions."+name+".base_url", fault.New(legacyconfig.ErrorCodeModelProtocolInvalid, err.Error())))
	}
	problems = append(problems, modelSecretProblems(name, definition, definition.BaseURL)...)
	problems = append(problems, modelCapabilityProblems(name, definition)...)
	return problems
}

func providerDefinitionProblems(models legacyconfig.Models, name string, definition *legacyconfig.ModelDefinition) []error {
	var problems []error
	if strings.TrimSpace(definition.Model) == "" {
		problems = append(problems, configkit.Field("models.definitions."+name, fault.New(legacyconfig.ErrorCodeModelProviderInvalid, "requires non-empty model")))
	}
	resolved, err := models.ResolveDefinition(definition)
	if err != nil {
		problems = append(problems, err)
		return append(problems, modelCapabilityProblems(name, definition)...)
	}
	problems = append(problems, modelSecretProblems(name, definition, resolved.BaseURL)...)
	problems = append(problems, modelCapabilityProblems(name, definition)...)
	return problems
}

func modelSecretProblems(name string, definition *legacyconfig.ModelDefinition, baseURL string) []error {
	var problems []error
	if definition.APIKeyRef.IsZero() && !legacyconfig.IsLoopbackBaseURL(baseURL) {
		problems = append(problems, configkit.Field("models.definitions."+name, fault.New(legacyconfig.ErrorCodeModelSecretInvalid, "requires api_key_ref for a non-loopback endpoint")))
	}
	return problems
}

func modelCapabilityProblems(name string, definition *legacyconfig.ModelDefinition) []error {
	var problems []error
	if definition.Capabilities.ContextTokens <= 0 || strings.TrimSpace(definition.Capabilities.Tokenizer) == "" {
		problems = append(problems, configkit.Field("models.definitions."+name, fault.New(legacyconfig.ErrorCodeModelCapabilityUnsupported, "requires positive context_tokens and a tokenizer")))
	}
	if definition.Capabilities.MicrosPerInputToken < 0 || definition.Capabilities.MicrosPerOutputToken < 0 {
		problems = append(problems, configkit.Field("models.definitions."+name+".capabilities", fault.New(legacyconfig.ErrorCodeModelCapabilityUnsupported, "cost rates must not be negative")))
	}
	return problems
}

func validateRouting(models legacyconfig.Models, doc *document) error {
	for task, role := range models.Routing {
		if role != "primary" && role != "auxiliary" {
			return configkit.Invalid("models.routing."+task, "role is not supported (must be primary or auxiliary)")
		}
		if doc.setByFileOrEnvironment(joinPath("models.routing", task)) && len(models.Definitions) > 0 {
			if _, ok := models.Definitions[role]; !ok {
				return configkit.Invalid("models.routing."+task, "references an undefined model")
			}
		}
	}
	return nil
}

func validateLoadedModelRoutes(routes map[string]legacyconfig.ModelRoute, models legacyconfig.Models) error {
	if len(routes) == 0 {
		return nil
	}
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(routes)) {
		route := routes[name]
		if !modelDefinitionNamePattern.MatchString(name) {
			problems = append(problems, configkit.Invalid("model_routes", fmt.Sprintf("invalid route name %q", name)))
		}
		if len(route.Candidates) < 1 {
			problems = append(problems, configkit.Invalid("model_routes."+name, "requires at least one candidate"))
		} else if len(route.Candidates) > 4 {
			problems = append(problems, configkit.Invalid("model_routes."+name, "exceeds maximum chain depth of 4 candidates"))
		}
		seen := make(map[string]bool, len(route.Candidates))
		for _, candidate := range route.Candidates {
			if seen[candidate] {
				problems = append(problems, configkit.Invalid("model_routes."+name, "contains a duplicate candidate"))
			}
			seen[candidate] = true
			if len(models.Definitions) > 0 {
				if _, ok := models.Definitions[candidate]; !ok {
					problems = append(problems, configkit.Invalid("model_routes."+name, "references unknown model definition"))
				}
			}
		}
		if route.MaxProviderAttempts < 0 {
			problems = append(problems, configkit.Invalid("model_routes."+name+".max_provider_attempts", "must not be negative"))
		}
		if route.RetryDelayBudget < 0 {
			problems = append(problems, configkit.Invalid("model_routes."+name+".retry_delay_budget", "must not be negative"))
		}
		if route.CostBudgetUSD < 0 {
			problems = append(problems, configkit.Invalid("model_routes."+name+".cost_budget_usd", "must not be negative"))
		}
		if route.Circuit.FailureThreshold < 0 {
			problems = append(problems, configkit.Invalid("model_routes."+name+".circuit.failure_threshold", "must not be negative"))
		}
		if route.Circuit.OpenDuration < 0 {
			problems = append(problems, configkit.Invalid("model_routes."+name+".circuit.open_duration", "must not be negative"))
		}
		if route.Circuit.MaxOpenDuration < 0 {
			problems = append(problems, configkit.Invalid("model_routes."+name+".circuit.max_open_duration", "must not be negative"))
		}
	}
	return errors.Join(problems...)
}

func decode(doc *document) (*Config, error) {
	if duplicates := duplicateKeys(doc.root, ""); len(duplicates) > 0 {
		sortProblems(duplicates)
		return nil, errors.Join(duplicates...)
	}
	var collector envCollector
	input, err := loadLayers(doc, &collector)
	if err != nil {
		return nil, err
	}
	documentProblems := doc.valueProblems()
	problems := slices.Concat(documentProblems, collector.errors(), reportUnknownKeys(doc, input))
	var cfg Config
	problems = append(problems, withoutReportedPaths(decodeProblems(doc, decodeInput(input, &cfg)), documentProblems)...)
	if len(problems) > 0 {
		sortProblems(problems)
		return nil, errors.Join(problems...)
	}
	return &cfg, nil
}

func decodeInput(input map[string]any, cfg *Config) error {
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           cfg,
		TagName:          "json",
		WeaklyTypedInput: false,
		ErrorUnused:      true,
		MatchName:        func(mapKey, fieldName string) bool { return mapKey == fieldName },
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			textValue(),
			integerValue(),
		),
	})
	if err != nil {
		return err
	}
	return decoder.Decode(input)
}

func screenOwnerSecrets(owner *legacyconfig.Owner, cfg *Config) error {
	if owner == nil {
		return nil
	}
	vault := configuredSecretVault(cfg)
	var problems []error
	for _, field := range []struct {
		path  string
		value string
	}{
		{"owner.name", owner.Name},
		{"owner.language", owner.Language},
		{"owner.timezone", owner.Timezone},
	} {
		if field.value != "" && vault.Contains(field.value) {
			problems = append(problems, configkit.Field(field.path, fault.New(legacyconfig.ErrorCodeOwnerSecret, "must not repeat a configured secret value")))
		}
	}
	for key, value := range owner.Preferences {
		if (key != "" && vault.Contains(key)) || (value != "" && vault.Contains(value)) {
			problems = append(problems, configkit.Field("owner.preferences", fault.New(legacyconfig.ErrorCodeOwnerSecret, "must not repeat a configured secret value")))
			break
		}
	}
	return errors.Join(problems...)
}

func configuredSecretVault(cfg *Config) *secret.Vault {
	vault := secret.New()
	for i, value := range configuredSecretValues(cfg) {
		vault.Set(fmt.Sprintf("configured-secret-%d", i), value)
	}
	return vault
}

func configuredSecretValues(cfg *Config) []string {
	var values []string
	add := func(value string) {
		if value = strings.TrimRight(value, "\r\n"); value != "" {
			values = append(values, value)
		}
	}
	for name := range cfg.Models.Definitions {
		definition := cfg.Models.Definitions[name]
		source := definition.APIKeyRef.Reference()
		if source.Env != "" {
			add(os.Getenv(source.Env))
		}
		if source.File != "" {
			if data, err := os.ReadFile(source.File); err == nil {
				add(string(data))
			}
		}
	}
	if cfg.Tools != nil {
		add(resolveSecretRef(cfg.Tools.WebSearch.CredentialRef))
		for i := range cfg.Tools.GitHub.Credentials {
			add(resolveSecretRef(cfg.Tools.GitHub.Credentials[i].Ref))
		}
	}
	for i := range cfg.Channels.Discord.Accounts {
		add(resolveSecretRef(cfg.Channels.Discord.Accounts[i].BotTokenRef))
	}
	for _, key := range cfg.Webhook.Keys {
		if key.SecretEnv != "" {
			add(os.Getenv(key.SecretEnv))
		}
	}
	add(resolveSecretRef(cfg.Telemetry.CredentialRef))
	if cfg.Sync != nil {
		add(resolveSecretRef(cfg.Sync.TransportSecretRef))
	}
	if cfg.MCP != nil {
		for i := range cfg.MCP.Servers {
			auth := cfg.MCP.Servers[i].Auth
			if auth == nil {
				continue
			}
			if oauth := auth.OAuth; oauth != nil {
				if oauth.ClientIDEnv != "" {
					add(os.Getenv(oauth.ClientIDEnv))
				}
				if oauth.ClientSecretEnv != "" {
					add(os.Getenv(oauth.ClientSecretEnv))
				}
			}
			if static := auth.Static; static != nil {
				add(resolveStaticCredential(static.CredentialRef))
			}
		}
	}
	return values
}

func resolveSecretRef(ref secret.Ref) string {
	value, _ := ref.Reference().Resolve()
	return value
}

func resolveStaticCredential(raw string) string {
	ref, err := secret.ParseRef(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return resolveSecretRef(ref)
}

func checkVersion(v int) error {
	if v != supportedVersion {
		return configkit.Field("version", fault.New(legacyconfig.ErrorCodeVersionUnsupported, fmt.Sprintf("unsupported version %d (supported: %d)", v, supportedVersion)))
	}
	return nil
}

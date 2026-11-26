package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-viper/mapstructure/v2"

	legacyconfig "github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/modules/codeexec/interpreter"
	"github.com/anggasct/aura/internal/modules/compaction"
	"github.com/anggasct/aura/internal/modules/knowledge"
	"github.com/anggasct/aura/internal/modules/mcp"
	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/skills"
	"github.com/anggasct/aura/internal/modules/workflow"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/capability"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sandbox"
	"github.com/anggasct/aura/internal/platform/secret"
)

const (
	envPrefix        = "AURA_"
	envConfigVar     = "AURA_CONFIG"
	supportedVersion = 1

	githubCapability = "provider-github"
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
	if err := validateGitHubCredentials(cfg, report.Active()); err != nil {
		return LoadResult{}, positionProblems(doc, err)
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

func validateLoaded(cfg *Config, doc *document, buildProfile capability.Profile) error {
	return errors.Join(
		configkit.Under("capabilities", cfg.Capabilities.Validate()),
		configkit.Under("models", cfg.Models.Validate()),
		configkit.Under("model_routes", model.ValidateRoutes(cfg.ModelRoutes)),
		legacyconfig.ValidateTools(cfg.Tools),
		validateSkills(cfg.Skills),
		validateContext(cfg.Context),
		validateProfile(cfg.Profile),
		legacyconfig.ValidateChildren(cfg.Children),
		validateAccess(cfg.Access),
		legacyconfig.ValidateSync(cfg.Sync),
		validateVision(cfg.Vision),
		validateKnowledge(cfg.Knowledge),
		configkit.Under("interpreter", cfg.Interpreter.Validate()),
		configkit.Under("runtime", cfg.Runtime.Validate()),
		configkit.Under("server", cfg.Server.Validate()),
		configkit.Under("logging", cfg.Logging.Validate()),
		configkit.Under("storage", cfg.Storage.Validate()),
		configkit.Under("telemetry", cfg.Telemetry.Validate()),
		configkit.Under("usage", cfg.Usage.Validate()),
		configkit.Under("health", cfg.Health.Validate()),
		legacyconfig.ValidateTerminal(cfg.Terminal),
		validateWorkflows(cfg.Workflows),
		validateMCP(cfg.MCP),
		validatePlugins(cfg.Plugins),
		configkit.Under("webhook", cfg.Webhook.Validate()),
		legacyconfig.ValidateDiscord(&cfg.Channels.Discord),
		legacyconfig.ValidateTelegram(&cfg.Channels.Telegram),
		legacyconfig.ValidateEmail(&cfg.Channels.Email),
		legacyconfig.ValidateHTTP(&cfg.Channels.HTTP),
		configkit.Under("broadcast", cfg.Broadcast.Validate()),
		configkit.Under("scheduler", cfg.Scheduler.Validate()),
		configkit.Under("memory", cfg.Memory.Validate()),
		configkit.Under("owner", principal.ValidateOwner(cfg.Owner)),
		validateAgents(cfg.Agents),
		configkit.Under("bindings", auraagent.ValidateBindings(cfg.Bindings)),
		validateAgainstBuild(cfg, buildProfile),
		validateReferences(cfg, doc),
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

func validateRouting(models model.Config, doc *document) error {
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

func screenOwnerSecrets(owner *principal.Owner, cfg *Config) error {
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
				add(resolveSecretRef(oauth.ClientIDRef))
				add(resolveSecretRef(oauth.ClientSecretRef))
			}
			if static := auth.Static; static != nil {
				add(resolveSecretRef(static.CredentialRef))
			}
		}
	}
	return values
}

func resolveSecretRef(ref secret.Ref) string {
	value, _ := ref.Reference().Resolve()
	return value
}

func validateMCP(section *mcp.Config) error {
	if section == nil {
		return nil
	}
	return configkit.Under("mcp", section.Validate())
}

func checkVersion(v int) error {
	if v != supportedVersion {
		return configkit.Field("version", fault.New(legacyconfig.ErrorCodeVersionUnsupported, fmt.Sprintf("unsupported version %d (supported: %d)", v, supportedVersion)))
	}
	return nil
}

func validateKnowledge(section *knowledge.Config) error {
	if section == nil {
		return nil
	}
	return configkit.Under("knowledge", section.Validate())
}

func validateContext(section *compaction.Config) error {
	if section == nil {
		return nil
	}
	return configkit.Under("context", section.Validate())
}

func validatePlugins(section *plugin.Config) error {
	if section == nil {
		return nil
	}
	return configkit.Under("plugins", section.Validate())
}

func validateSkills(section *skills.Config) error {
	if section == nil {
		return nil
	}
	return configkit.Under("skills", section.Validate())
}

func validateProfile(section *profile.Config) error {
	if section == nil {
		return nil
	}
	return configkit.Under("profile", section.Validate())
}

func validateWorkflows(section *workflow.Config) error {
	if section == nil {
		return nil
	}
	return configkit.Under("workflows", section.Validate())
}

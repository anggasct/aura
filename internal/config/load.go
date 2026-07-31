package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/anggasct/aura/internal/capability"
)

const (
	envPrefix        = "AURA_"
	envConfigVar     = "AURA_CONFIG"
	supportedVersion = 1
)

var (
	envLookup  = buildEnvLookup()
	yamlLineRe = regexp.MustCompile(`line (\d+)`)
)

type LoadResult struct {
	Config           *Config
	Path             string
	DefaultGenerated bool
	CapabilityReport capability.Report
}

type LoadOptions struct {
	Build        capability.Build
	Registry     capability.Registry
	Dependencies capability.Dependencies
}

func buildEnvLookup() map[string]string {
	paths, _, _ := validKeyPaths()
	m := map[string]string{}
	for path := range paths {
		m[strings.ReplaceAll(path, ".", "_")] = path
	}
	return m
}

func envKeyMapper(s string) string {
	normalized := strings.ToLower(strings.TrimPrefix(s, envPrefix))
	if path := envLookup[normalized]; path != "" {
		return path
	}
	const definitionsPrefix = "models_definitions_"
	if !strings.HasPrefix(normalized, definitionsPrefix) {
		return ""
	}
	remainder := strings.TrimPrefix(normalized, definitionsPrefix)
	fields := []struct {
		suffix string
		path   string
	}{
		{suffix: "_capabilities_streaming", path: "capabilities.streaming"},
		{suffix: "_capabilities_tools", path: "capabilities.tools"},
		{suffix: "_capabilities_structured_output", path: "capabilities.structured_output"},
		{suffix: "_capabilities_vision", path: "capabilities.vision"},
		{suffix: "_capabilities_audio", path: "capabilities.audio"},
		{suffix: "_capabilities_reasoning", path: "capabilities.reasoning"},
		{suffix: "_capabilities_context_tokens", path: "capabilities.context_tokens"},
		{suffix: "_capabilities_tokenizer", path: "capabilities.tokenizer"},
		{suffix: "_capabilities_usage_reporting", path: "capabilities.usage_reporting"},
		{suffix: "_api_key_env", path: "api_key_env"},
		{suffix: "_api_key_file", path: "api_key_file"},
		{suffix: "_base_url", path: "base_url"},
		{suffix: "_protocol", path: "protocol"},
		{suffix: "_model", path: "model"},
	}
	for _, field := range fields {
		if !strings.HasSuffix(remainder, field.suffix) {
			continue
		}
		name := strings.TrimSuffix(remainder, field.suffix)
		if modelDefinitionNamePattern.MatchString(name) {
			return "models.definitions." + name + "." + field.path
		}
	}
	return ""
}

// Load reads configuration. Path precedence: an explicit path argument, then
// AURA_CONFIG, then the default XDG location. A missing default config is
// auto-generated; a missing explicit or AURA_CONFIG path is an error.
// AURA_-prefixed environment variables override file values. DefaultGenerated
// is true when a default config was written, so callers can log it through the
// configured logger after setup.
func Load(path string) (LoadResult, error) {
	build, err := capability.CurrentBuild()
	if err != nil {
		return LoadResult{}, fmt.Errorf("config: %w", err)
	}
	return LoadWithOptions(path, LoadOptions{Build: build, Registry: capability.EmptyRegistry()})
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
	if err := validate(data); err != nil {
		return LoadResult{}, err
	}
	cfg, err := decode(data)
	if err != nil {
		return LoadResult{}, err
	}
	if err := checkVersion(cfg.Version); err != nil {
		return LoadResult{}, err
	}
	if err := applyDefaults(cfg, data); err != nil {
		return LoadResult{}, err
	}
	if err := validateLoadedModels(cfg.Models); err != nil {
		return LoadResult{}, err
	}
	if err := validateRuntime(cfg.Runtime); err != nil {
		return LoadResult{}, err
	}
	report, err := options.Registry.Resolve(options.Build, cfg.Capabilities.Enabled, options.Dependencies)
	if err != nil {
		return LoadResult{}, err
	}
	return LoadResult{
		Config:           cfg,
		Path:             resolved,
		DefaultGenerated: generated,
		CapabilityReport: report,
	}, nil
}

func resolvePath(flagPath string) (string, bool, error) {
	if flagPath != "" {
		return flagPath, true, nil
	}
	if env := os.Getenv(envConfigVar); env != "" {
		return env, true, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", false, fmt.Errorf("cannot resolve config directory: set HOME or XDG_CONFIG_HOME: %w", err)
	}
	return filepath.Join(base, "aura", "config.yaml"), false, nil
}

func loadBytes(path string, explicit bool) ([]byte, bool, error) {
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
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return nil, false, fmt.Errorf("cannot read %s: permission denied", path)
		}
		return nil, false, fmt.Errorf("cannot read %s: %w", path, err)
	}
	return data, false, nil
}

func writeDefault(path string) ([]byte, error) {
	data, err := Marshal(Default())
	if err != nil {
		return nil, fmt.Errorf("failed to marshal default config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("cannot create config directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, fmt.Errorf("cannot write default config: %w", err)
	}
	return data, nil
}

func validate(data []byte) error {
	doc, err := parseDocument(data)
	if err != nil {
		return err
	}
	version, err := documentVersion(doc)
	if err != nil {
		return err
	}
	if err := checkVersion(version); err != nil {
		return err
	}
	if err := validateRuntimeAndCapabilityShapes(doc); err != nil {
		return err
	}
	if err := validateModelShapes(doc); err != nil {
		return err
	}
	valid, mapPaths, structMapPaths := validKeyPaths()
	return checkUnknownKeys(doc, "", valid, mapPaths, structMapPaths)
}

func parseDocument(data []byte) (*yamlv3.Node, error) {
	var root yamlv3.Node
	if err := yamlv3.Unmarshal(data, &root); err != nil {
		return nil, malformedYAMLError(err)
	}
	if root.Kind != yamlv3.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("invalid YAML: empty document")
	}
	doc := root.Content[0]
	if doc.Kind != yamlv3.MappingNode {
		return nil, fmt.Errorf("invalid YAML: top level must be a mapping")
	}
	return doc, nil
}

func documentVersion(doc *yamlv3.Node) (int, error) {
	found := false
	version := 0
	for i := 0; i+1 < len(doc.Content); i += 2 {
		keyNode := doc.Content[i]
		if keyNode.Value != "version" {
			continue
		}
		if found {
			return 0, fmt.Errorf("duplicate key %q at line %d", "version", keyNode.Line)
		}
		found = true
		if err := doc.Content[i+1].Decode(&version); err != nil {
			return 0, fmt.Errorf("invalid version at line %d: %w", doc.Content[i+1].Line, err)
		}
	}
	return version, nil
}

func validateRuntimeAndCapabilityShapes(doc *yamlv3.Node) error {
	if runtimeNode := mappingValue(doc, "runtime"); runtimeNode != nil {
		if runtimeNode.Kind != yamlv3.MappingNode {
			return fmt.Errorf("runtime must be a mapping at line %d", runtimeNode.Line)
		}
		for i := 0; i+1 < len(runtimeNode.Content); i += 2 {
			keyNode := runtimeNode.Content[i]
			valueNode := runtimeNode.Content[i+1]
			switch keyNode.Value {
			case "max_active_turns", "max_pending_turns":
				if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!int" {
					return fmt.Errorf("runtime.%s must be an integer at line %d", keyNode.Value, valueNode.Line)
				}
			case "turn_timeout", "shutdown_timeout":
				if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
					return fmt.Errorf("runtime.%s must be a duration string at line %d", keyNode.Value, valueNode.Line)
				}
			}
		}
	}

	capabilitiesNode := mappingValue(doc, "capabilities")
	if capabilitiesNode == nil {
		return nil
	}
	if capabilitiesNode.Kind != yamlv3.MappingNode {
		return fmt.Errorf("capabilities must be a mapping at line %d", capabilitiesNode.Line)
	}
	enabledNode := mappingValue(capabilitiesNode, "enabled")
	if enabledNode == nil {
		return nil
	}
	if enabledNode.Kind != yamlv3.SequenceNode {
		return fmt.Errorf("capabilities.enabled must be a sequence at line %d", enabledNode.Line)
	}
	seen := make(map[string]struct{}, len(enabledNode.Content))
	for _, item := range enabledNode.Content {
		if item.Kind != yamlv3.ScalarNode || item.Tag != "!!str" {
			return fmt.Errorf("capabilities.enabled values must be strings at line %d", item.Line)
		}
		if _, exists := seen[item.Value]; exists {
			return fmt.Errorf("duplicate capability %q at line %d", item.Value, item.Line)
		}
		seen[item.Value] = struct{}{}
	}
	return nil
}

func mappingValue(node *yamlv3.Node, key string) *yamlv3.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func validateModelShapes(doc *yamlv3.Node) error {
	modelsNode := mappingValue(doc, "models")
	if modelsNode == nil {
		return nil
	}
	if modelsNode.Kind != yamlv3.MappingNode {
		return fmt.Errorf("models must be a mapping at line %d", modelsNode.Line)
	}
	definitionsNode := mappingValue(modelsNode, "definitions")
	if definitionsNode == nil {
		return nil
	}
	if definitionsNode.Kind != yamlv3.MappingNode {
		return fmt.Errorf("models.definitions must be a mapping at line %d", definitionsNode.Line)
	}
	for i := 0; i+1 < len(definitionsNode.Content); i += 2 {
		nameNode := definitionsNode.Content[i]
		definitionNode := definitionsNode.Content[i+1]
		if !modelDefinitionNamePattern.MatchString(nameNode.Value) {
			return fmt.Errorf("invalid model definition name %q at line %d", nameNode.Value, nameNode.Line)
		}
		if definitionNode.Kind != yamlv3.MappingNode {
			return fmt.Errorf("models.definitions.%s must be a mapping at line %d", nameNode.Value, definitionNode.Line)
		}
		if err := validateModelDefinition(nameNode.Value, definitionNode); err != nil {
			return err
		}
	}
	return nil
}

func validateModelDefinition(name string, node *yamlv3.Node) error {
	protocolNode := mappingValue(node, "protocol")
	modelNode := mappingValue(node, "model")
	if protocolNode == nil || modelNode == nil || protocolNode.Kind != yamlv3.ScalarNode || modelNode.Kind != yamlv3.ScalarNode || protocolNode.Tag != "!!str" || modelNode.Tag != "!!str" || strings.TrimSpace(protocolNode.Value) == "" || strings.TrimSpace(modelNode.Value) == "" {
		return fmt.Errorf("models.definitions.%s requires non-empty protocol and model strings", name)
	}
	if !validProtocol(protocolNode.Value) {
		return &Error{Code: ErrorCodeModelProtocolInvalid, Detail: fmt.Sprintf("models.definitions.%s uses unsupported protocol %q", name, protocolNode.Value)}
	}

	baseURL := ""
	if node := mappingValue(node, "base_url"); node != nil {
		if node.Kind != yamlv3.ScalarNode || node.Tag != "!!str" {
			return fmt.Errorf("models.definitions.%s.base_url must be a string at line %d", name, node.Line)
		}
		baseURL = node.Value
	}
	if err := validateModelBaseURL(baseURL); err != nil {
		return &Error{Code: ErrorCodeModelProtocolInvalid, Detail: fmt.Sprintf("models.definitions.%s.base_url: %v", name, err)}
	}

	apiKeyEnv := scalarString(mappingValue(node, "api_key_env"))
	apiKeyFile := scalarString(mappingValue(node, "api_key_file"))
	if apiKeyEnv != "" && apiKeyFile != "" {
		return &Error{Code: ErrorCodeModelSecretInvalid, Detail: fmt.Sprintf("models.definitions.%s must set only one of api_key_env or api_key_file", name)}
	}
	if apiKeyEnv != "" && !envNamePattern.MatchString(apiKeyEnv) {
		return &Error{Code: ErrorCodeModelSecretInvalid, Detail: fmt.Sprintf("models.definitions.%s.api_key_env is not a valid environment variable name", name)}
	}
	if apiKeyEnv == "" && apiKeyFile == "" && !isLoopbackBaseURL(baseURL) {
		return &Error{Code: ErrorCodeModelSecretInvalid, Detail: fmt.Sprintf("models.definitions.%s requires api_key_env or api_key_file for a non-loopback endpoint", name)}
	}

	capabilitiesNode := mappingValue(node, "capabilities")
	if capabilitiesNode == nil || capabilitiesNode.Kind != yamlv3.MappingNode {
		return &Error{Code: ErrorCodeModelCapabilityUnsupported, Detail: fmt.Sprintf("models.definitions.%s requires a capabilities mapping", name)}
	}
	for _, key := range []string{"streaming", "tools", "structured_output", "vision", "audio", "reasoning", "usage_reporting"} {
		valueNode := mappingValue(capabilitiesNode, key)
		if valueNode == nil || valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!bool" {
			return &Error{Code: ErrorCodeModelCapabilityUnsupported, Detail: fmt.Sprintf("models.definitions.%s.capabilities.%s must be a boolean", name, key)}
		}
	}
	contextNode := mappingValue(capabilitiesNode, "context_tokens")
	contextTokens, contextErr := strconv.ParseInt("0", 10, 64)
	if contextNode != nil && contextNode.Kind == yamlv3.ScalarNode && contextNode.Tag == "!!int" {
		contextTokens, contextErr = strconv.ParseInt(contextNode.Value, 0, 64)
	}
	if contextNode == nil || contextNode.Kind != yamlv3.ScalarNode || contextNode.Tag != "!!int" || contextErr != nil || contextTokens <= 0 {
		return &Error{Code: ErrorCodeModelCapabilityUnsupported, Detail: fmt.Sprintf("models.definitions.%s.capabilities.context_tokens must be positive", name)}
	}
	tokenizerNode := mappingValue(capabilitiesNode, "tokenizer")
	if tokenizerNode == nil || tokenizerNode.Kind != yamlv3.ScalarNode || tokenizerNode.Tag != "!!str" || strings.TrimSpace(tokenizerNode.Value) == "" {
		return &Error{Code: ErrorCodeModelCapabilityUnsupported, Detail: fmt.Sprintf("models.definitions.%s.capabilities.tokenizer must be a non-empty string", name)}
	}
	return nil
}

func validateLoadedModels(models Models) error {
	for name, definition := range models.Definitions {
		if !validProtocol(definition.Protocol) {
			return &Error{Code: ErrorCodeModelProtocolInvalid, Detail: fmt.Sprintf("models.definitions.%s uses unsupported protocol %q", name, definition.Protocol)}
		}
		if err := validateModelBaseURL(definition.BaseURL); err != nil {
			return &Error{Code: ErrorCodeModelProtocolInvalid, Detail: fmt.Sprintf("models.definitions.%s.base_url: %v", name, err)}
		}
		if definition.APIKeyEnv != "" && definition.APIKeyFile != "" {
			return &Error{Code: ErrorCodeModelSecretInvalid, Detail: fmt.Sprintf("models.definitions.%s must set only one of api_key_env or api_key_file", name)}
		}
		if definition.APIKeyEnv != "" && !envNamePattern.MatchString(definition.APIKeyEnv) {
			return &Error{Code: ErrorCodeModelSecretInvalid, Detail: fmt.Sprintf("models.definitions.%s.api_key_env is not a valid environment variable name", name)}
		}
		if definition.APIKeyEnv == "" && definition.APIKeyFile == "" && !isLoopbackBaseURL(definition.BaseURL) {
			return &Error{Code: ErrorCodeModelSecretInvalid, Detail: fmt.Sprintf("models.definitions.%s requires api_key_env or api_key_file for a non-loopback endpoint", name)}
		}
		if definition.Capabilities.ContextTokens <= 0 || strings.TrimSpace(definition.Capabilities.Tokenizer) == "" {
			return &Error{Code: ErrorCodeModelCapabilityUnsupported, Detail: fmt.Sprintf("models.definitions.%s requires positive context_tokens and a tokenizer", name)}
		}
	}
	return nil
}

func scalarString(node *yamlv3.Node) string {
	if node == nil || node.Kind != yamlv3.ScalarNode || node.Tag != "!!str" {
		return ""
	}
	return node.Value
}

func isLoopbackBaseURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && isLoopbackURL(u)
}

func malformedYAMLError(err error) error {
	if m := yamlLineRe.FindStringSubmatch(err.Error()); len(m) == 2 {
		line, _ := strconv.Atoi(m[1])
		return fmt.Errorf("invalid YAML at line %d: %w", line, err)
	}
	return fmt.Errorf("invalid YAML: %w", err)
}

func checkUnknownKeys(node *yamlv3.Node, prefix string, valid, mapPaths, structMapPaths map[string]bool) error {
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		path := keyNode.Value
		if prefix != "" {
			path = prefix + "." + path
		}
		if _, exists := seen[path]; exists {
			return fmt.Errorf("duplicate key %q at line %d", path, keyNode.Line)
		}
		seen[path] = struct{}{}
		if !validConfigPath(path, valid, structMapPaths) {
			return fmt.Errorf("unknown key %q at line %d", path, keyNode.Line)
		}
		if valNode.Kind == yamlv3.MappingNode {
			if structMapPaths[path] {
				if err := checkUnknownKeys(valNode, path, valid, mapPaths, structMapPaths); err != nil {
					return err
				}
			} else if !mapPaths[path] {
				if err := checkUnknownKeys(valNode, path, valid, mapPaths, structMapPaths); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validConfigPath(path string, valid map[string]bool, structMapPaths map[string]bool) bool {
	if valid[path] {
		return true
	}
	for mapPath := range structMapPaths {
		prefix := mapPath + "."
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(path, prefix)
		if !strings.Contains(remainder, ".") {
			return true
		}
		if dot := strings.IndexByte(remainder, '.'); dot >= 0 {
			return valid[mapPath+".*"+remainder[dot:]]
		}
	}
	return false
}

func decode(data []byte) (*Config, error) {
	k := koanf.New(".")
	if err := k.Load(rawbytes.Provider(data), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if err := k.Load(env.Provider(envPrefix, ".", envKeyMapper), nil); err != nil {
		return nil, fmt.Errorf("failed to load environment overrides: %w", err)
	}
	var cfg Config
	err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			Result:           &cfg,
			WeaklyTypedInput: true,
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				stringToDurationHook(),
				stringToStringSliceHook(),
			),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to decode config: %w", err)
	}
	return &cfg, nil
}

func stringToDurationHook() mapstructure.DecodeHookFunc {
	return func(from, to reflect.Type, data interface{}) (interface{}, error) {
		if to != reflect.TypeOf(Duration(0)) {
			return data, nil
		}
		s, ok := data.(string)
		if !ok {
			return data, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		return Duration(d), nil
	}
}

func stringToStringSliceHook() mapstructure.DecodeHookFunc {
	return func(from, to reflect.Type, data interface{}) (interface{}, error) {
		if from.Kind() != reflect.String || to.Kind() != reflect.Slice || to.Elem().Kind() != reflect.String {
			return data, nil
		}
		s, ok := data.(string)
		if !ok {
			return data, nil
		}
		if strings.TrimSpace(s) == "" {
			return []string{}, nil
		}
		values := strings.Split(s, ",")
		for i := range values {
			values[i] = strings.TrimSpace(values[i])
			if values[i] == "" {
				return nil, fmt.Errorf("invalid empty list value in %q", s)
			}
		}
		return values, nil
	}
}

func applyDefaults(cfg *Config, data []byte) error {
	doc, err := parseDocument(data)
	if err != nil {
		return err
	}
	defaults := Default()
	if cfg.Runtime.MaxActiveTurns == 0 && !configValuePresent(doc, "runtime", "max_active_turns") && !envValuePresent("runtime.max_active_turns") {
		cfg.Runtime.MaxActiveTurns = defaults.Runtime.MaxActiveTurns
	}
	if cfg.Runtime.MaxPendingTurns == 0 && !configValuePresent(doc, "runtime", "max_pending_turns") && !envValuePresent("runtime.max_pending_turns") {
		cfg.Runtime.MaxPendingTurns = defaults.Runtime.MaxPendingTurns
	}
	if cfg.Runtime.TurnTimeout == 0 && !configValuePresent(doc, "runtime", "turn_timeout") && !envValuePresent("runtime.turn_timeout") {
		cfg.Runtime.TurnTimeout = defaults.Runtime.TurnTimeout
	}
	if cfg.Runtime.ShutdownTimeout == 0 && !configValuePresent(doc, "runtime", "shutdown_timeout") && !envValuePresent("runtime.shutdown_timeout") {
		cfg.Runtime.ShutdownTimeout = defaults.Runtime.ShutdownTimeout
	}
	if cfg.Capabilities.Enabled == nil {
		cfg.Capabilities.Enabled = []string{}
	}
	if cfg.Server.Host == "" {
		cfg.Server.Host = defaults.Server.Host
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = defaults.Server.Port
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = defaults.Logging.Level
	}
	if cfg.Logging.Format == "" {
		cfg.Logging.Format = defaults.Logging.Format
	}
	return nil
}

func configValuePresent(node *yamlv3.Node, path ...string) bool {
	for _, key := range path {
		node = mappingValue(node, key)
		if node == nil {
			return false
		}
	}
	return true
}

func envValuePresent(path string) bool {
	key := envPrefix + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
	_, ok := os.LookupEnv(key)
	return ok
}

func validateRuntime(runtime Runtime) error {
	if runtime.MaxActiveTurns <= 0 {
		return fmt.Errorf("runtime.max_active_turns must be positive")
	}
	if runtime.MaxPendingTurns < runtime.MaxActiveTurns {
		return fmt.Errorf("runtime.max_pending_turns must be at least runtime.max_active_turns")
	}
	if runtime.TurnTimeout <= 0 {
		return fmt.Errorf("runtime.turn_timeout must be positive")
	}
	if runtime.ShutdownTimeout <= 0 {
		return fmt.Errorf("runtime.shutdown_timeout must be positive")
	}
	return nil
}

func checkVersion(v int) error {
	if v != supportedVersion {
		return &Error{
			Code:   ErrorCodeVersionUnsupported,
			Detail: fmt.Sprintf("unsupported version %q (supported: %d)", strconv.Itoa(v), supportedVersion),
		}
	}
	return nil
}

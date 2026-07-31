package config

import (
	"errors"
	"fmt"
	"io/fs"
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
	paths, _ := validKeyPaths()
	m := map[string]string{}
	for path := range paths {
		m[strings.ReplaceAll(path, ".", "_")] = path
	}
	return m
}

func envKeyMapper(s string) string {
	return envLookup[strings.ToLower(strings.TrimPrefix(s, envPrefix))]
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
	valid, mapPaths := validKeyPaths()
	return checkUnknownKeys(doc, "", valid, mapPaths)
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

func malformedYAMLError(err error) error {
	if m := yamlLineRe.FindStringSubmatch(err.Error()); len(m) == 2 {
		line, _ := strconv.Atoi(m[1])
		return fmt.Errorf("invalid YAML at line %d: %w", line, err)
	}
	return fmt.Errorf("invalid YAML: %w", err)
}

func checkUnknownKeys(node *yamlv3.Node, prefix string, valid, mapPaths map[string]bool) error {
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
		if !valid[path] {
			return fmt.Errorf("unknown key %q at line %d", path, keyNode.Line)
		}
		if valNode.Kind == yamlv3.MappingNode && !mapPaths[path] {
			if err := checkUnknownKeys(valNode, path, valid, mapPaths); err != nil {
				return err
			}
		}
	}
	return nil
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

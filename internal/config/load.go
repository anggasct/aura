package config

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	"github.com/anggasct/aura/internal/logging"
)

const (
	envPrefix        = "AURA_"
	envConfigVar     = "AURA_CONFIG"
	supportedVersion = 1
)

var envLookup = buildEnvLookup()

type LoadResult struct {
	Config           *Config
	Path             string
	DefaultGenerated bool
	CapabilityReport capability.Report
	Warnings         []string
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
	if err := validateLoadedModels(cfg.Models, routingExplicitIn(data)); err != nil {
		return LoadResult{}, err
	}
	if err := validateTools(cfg.Tools, options.Build.Profile()); err != nil {
		return LoadResult{}, err
	}
	if err := validateRuntime(cfg.Runtime); err != nil {
		return LoadResult{}, err
	}
	if err := validateServer(cfg.Server); err != nil {
		return LoadResult{}, err
	}
	if err := validateLogging(cfg.Logging); err != nil {
		return LoadResult{}, err
	}
	if err := validateStorage(cfg.Storage); err != nil {
		return LoadResult{}, err
	}
	if err := validateTelemetry(cfg.Telemetry); err != nil {
		return LoadResult{}, err
	}
	if err := validateUsage(cfg.Usage); err != nil {
		return LoadResult{}, err
	}
	if err := validateHealth(cfg.Health); err != nil {
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
		Warnings:         unknownEnvKeys(),
	}, nil
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
	if err := validateSectionShapes(doc); err != nil {
		return err
	}
	if err := validateTelemetryShapes(doc); err != nil {
		return err
	}
	if err := validateHealthShapes(doc); err != nil {
		return err
	}
	if err := validateUsageShapes(doc); err != nil {
		return err
	}
	if err := validateModelShapes(doc); err != nil {
		return err
	}
	if err := validateToolsShapes(doc); err != nil {
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
		return nil, errors.New("invalid YAML: empty document")
	}
	doc := root.Content[0]
	if doc.Kind != yamlv3.MappingNode {
		return nil, errors.New("invalid YAML: top level must be a mapping")
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

func validateSectionShapes(doc *yamlv3.Node) error {
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

	if serverNode := mappingValue(doc, "server"); serverNode != nil {
		if serverNode.Kind != yamlv3.MappingNode {
			return fmt.Errorf("server must be a mapping at line %d", serverNode.Line)
		}
		for i := 0; i+1 < len(serverNode.Content); i += 2 {
			keyNode := serverNode.Content[i]
			valueNode := serverNode.Content[i+1]
			switch keyNode.Value {
			case "host":
				if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
					return fmt.Errorf("server.host must be a string at line %d", valueNode.Line)
				}
			case "port":
				if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!int" {
					return fmt.Errorf("server.port must be an integer at line %d", valueNode.Line)
				}
			}
		}
	}

	if loggingNode := mappingValue(doc, "logging"); loggingNode != nil {
		if loggingNode.Kind != yamlv3.MappingNode {
			return fmt.Errorf("logging must be a mapping at line %d", loggingNode.Line)
		}
		for i := 0; i+1 < len(loggingNode.Content); i += 2 {
			keyNode := loggingNode.Content[i]
			valueNode := loggingNode.Content[i+1]
			switch keyNode.Value {
			case "level", "format":
				if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
					return fmt.Errorf("logging.%s must be a string at line %d", keyNode.Value, valueNode.Line)
				}
			}
		}
	}

	if modelsNode := mappingValue(doc, "models"); modelsNode != nil {
		if modelsNode.Kind != yamlv3.MappingNode {
			return fmt.Errorf("models must be a mapping at line %d", modelsNode.Line)
		}
		for i := 0; i+1 < len(modelsNode.Content); i += 2 {
			keyNode := modelsNode.Content[i]
			valueNode := modelsNode.Content[i+1]
			switch keyNode.Value {
			case "request_timeout", "streaming_idle_timeout":
				if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
					return fmt.Errorf("models.%s must be a duration string at line %d", keyNode.Value, valueNode.Line)
				}
			case "routing":
				if valueNode.Kind != yamlv3.MappingNode {
					return fmt.Errorf("models.routing must be a mapping at line %d", valueNode.Line)
				}
				for j := 0; j+1 < len(valueNode.Content); j += 2 {
					roleNode := valueNode.Content[j+1]
					if roleNode.Kind != yamlv3.ScalarNode || roleNode.Tag != "!!str" {
						return fmt.Errorf("models.routing values must be strings at line %d", roleNode.Line)
					}
				}
			}
		}
	}

	if storageNode := mappingValue(doc, "storage"); storageNode != nil {
		if storageNode.Kind != yamlv3.MappingNode {
			return fmt.Errorf("storage must be a mapping at line %d", storageNode.Line)
		}
		for i := 0; i+1 < len(storageNode.Content); i += 2 {
			keyNode := storageNode.Content[i]
			valueNode := storageNode.Content[i+1]
			switch keyNode.Value {
			case "path", "backup_directory":
				if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
					return fmt.Errorf("storage.%s must be a string at line %d", keyNode.Value, valueNode.Line)
				}
			case "busy_timeout", "backup_interval":
				if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
					return fmt.Errorf("storage.%s must be a duration string at line %d", keyNode.Value, valueNode.Line)
				}
			case "max_open_connections", "backup_retention":
				if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!int" {
					return fmt.Errorf("storage.%s must be an integer at line %d", keyNode.Value, valueNode.Line)
				}
			case "artifact_quota":
				if valueNode.Kind != yamlv3.ScalarNode || (valueNode.Tag != "!!str" && valueNode.Tag != "!!int") {
					return fmt.Errorf("storage.artifact_quota must be a size string at line %d", valueNode.Line)
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

func validateTelemetryShapes(doc *yamlv3.Node) error {
	telemetryNode := mappingValue(doc, "telemetry")
	if telemetryNode == nil {
		return nil
	}
	if telemetryNode.Kind != yamlv3.MappingNode {
		return fmt.Errorf("telemetry must be a mapping at line %d", telemetryNode.Line)
	}
	for i := 0; i+1 < len(telemetryNode.Content); i += 2 {
		keyNode := telemetryNode.Content[i]
		valueNode := telemetryNode.Content[i+1]
		switch keyNode.Value {
		case "exporter", "endpoint", "credential_ref":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("telemetry.%s must be a string at line %d", keyNode.Value, valueNode.Line)
			}
		case "sample_ratio":
			if valueNode.Kind != yamlv3.ScalarNode || (valueNode.Tag != "!!float" && valueNode.Tag != "!!int") {
				return fmt.Errorf("telemetry.sample_ratio must be a number at line %d", valueNode.Line)
			}
		case "queue_size":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!int" {
				return fmt.Errorf("telemetry.queue_size must be an integer at line %d", valueNode.Line)
			}
		case "export_timeout":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("telemetry.export_timeout must be a duration string at line %d", valueNode.Line)
			}
		}
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

func validateHealthShapes(doc *yamlv3.Node) error {
	healthNode := mappingValue(doc, "health")
	if healthNode == nil {
		return nil
	}
	if healthNode.Kind != yamlv3.MappingNode {
		return fmt.Errorf("health must be a mapping at line %d", healthNode.Line)
	}
	for i := 0; i+1 < len(healthNode.Content); i += 2 {
		keyNode := healthNode.Content[i]
		valueNode := healthNode.Content[i+1]
		switch keyNode.Value {
		case "listen":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("health.listen must be a string at line %d", valueNode.Line)
			}
		case "check_interval", "check_timeout", "backup_max_age", "restore_verification_max_age":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("health.%s must be a duration string at line %d", keyNode.Value, valueNode.Line)
			}
		case "disk_warning_percent", "disk_critical_percent":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!int" {
				return fmt.Errorf("health.%s must be an integer at line %d", keyNode.Value, valueNode.Line)
			}
		case "disk_critical_floor_bytes":
			if valueNode.Kind != yamlv3.ScalarNode || (valueNode.Tag != "!!int" && valueNode.Tag != "!!str") {
				return fmt.Errorf("health.disk_critical_floor_bytes must be a byte size at line %d", valueNode.Line)
			}
		}
	}
	return nil
}

func validateUsageShapes(doc *yamlv3.Node) error {
	usageNode := mappingValue(doc, "usage")
	if usageNode == nil {
		return nil
	}
	if usageNode.Kind != yamlv3.MappingNode {
		return fmt.Errorf("usage must be a mapping at line %d", usageNode.Line)
	}
	for i := 0; i+1 < len(usageNode.Content); i += 2 {
		keyNode := usageNode.Content[i]
		valueNode := usageNode.Content[i+1]
		switch keyNode.Value {
		case "currency":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("usage.currency must be a string at line %d", valueNode.Line)
			}
		case "daily_budget_micros", "monthly_budget_micros":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!int" {
				return fmt.Errorf("usage.%s must be an integer at line %d", keyNode.Value, valueNode.Line)
			}
		case "reservation_ttl":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("usage.reservation_ttl must be a duration string at line %d", valueNode.Line)
			}
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

func validateToolsShapes(doc *yamlv3.Node) error {
	toolsNode := mappingValue(doc, "tools")
	if toolsNode == nil {
		return nil
	}
	if toolsNode.Kind != yamlv3.MappingNode {
		return fmt.Errorf("tools must be a mapping at line %d", toolsNode.Line)
	}
	for i := 0; i+1 < len(toolsNode.Content); i += 2 {
		keyNode := toolsNode.Content[i]
		valueNode := toolsNode.Content[i+1]
		switch keyNode.Value {
		case "workspace":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("tools.workspace must be a string at line %d", valueNode.Line)
			}
		case "max_inline_result_bytes":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!int" {
				return fmt.Errorf("tools.max_inline_result_bytes must be an integer at line %d", valueNode.Line)
			}
		case "exec":
			if err := validateToolExecShape(valueNode); err != nil {
				return err
			}
		case "fetch":
			if err := validateToolFetchShape(valueNode); err != nil {
				return err
			}
		case "web_search":
			if err := validateToolWebSearchShape(valueNode); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateToolExecShape(node *yamlv3.Node) error {
	if node.Kind != yamlv3.MappingNode {
		return fmt.Errorf("tools.exec must be a mapping at line %d", node.Line)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valueNode := node.Content[i+1]
		switch keyNode.Value {
		case "timeout":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("tools.exec.timeout must be a duration string at line %d", valueNode.Line)
			}
		case "max_stdout_bytes", "max_stderr_bytes":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!int" {
				return fmt.Errorf("tools.exec.%s must be an integer at line %d", keyNode.Value, valueNode.Line)
			}
		}
	}
	return nil
}

func validateToolFetchShape(node *yamlv3.Node) error {
	if node.Kind != yamlv3.MappingNode {
		return fmt.Errorf("tools.fetch must be a mapping at line %d", node.Line)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valueNode := node.Content[i+1]
		switch keyNode.Value {
		case "timeout":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("tools.fetch.timeout must be a duration string at line %d", valueNode.Line)
			}
		case "max_redirects":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!int" {
				return fmt.Errorf("tools.fetch.max_redirects must be an integer at line %d", valueNode.Line)
			}
		case "max_encoded_bytes", "max_decoded_bytes":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!int" {
				return fmt.Errorf("tools.fetch.%s must be an integer at line %d", keyNode.Value, valueNode.Line)
			}
		}
	}
	return nil
}

func validateToolWebSearchShape(node *yamlv3.Node) error {
	if node.Kind != yamlv3.MappingNode {
		return fmt.Errorf("tools.web_search must be a mapping at line %d", node.Line)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valueNode := node.Content[i+1]
		switch keyNode.Value {
		case "provider", "credential_ref":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("tools.web_search.%s must be a string at line %d", keyNode.Value, valueNode.Line)
			}
		case "max_results":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!int" {
				return fmt.Errorf("tools.web_search.max_results must be an integer at line %d", valueNode.Line)
			}
		}
	}
	return nil
}

// validateModelDefinition checks only the shape of one model definition;
// all semantic rules live in validateLoadedModels.
func validateModelDefinition(name string, node *yamlv3.Node) error {
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valueNode := node.Content[i+1]
		switch keyNode.Value {
		case "protocol", "model", "base_url", "api_key_env", "api_key_file":
			if valueNode.Kind != yamlv3.ScalarNode || valueNode.Tag != "!!str" {
				return fmt.Errorf("models.definitions.%s.%s must be a string at line %d", name, keyNode.Value, valueNode.Line)
			}
		case "capabilities":
			if valueNode.Kind != yamlv3.MappingNode {
				return fmt.Errorf("models.definitions.%s.capabilities must be a mapping at line %d", name, valueNode.Line)
			}
			for j := 0; j+1 < len(valueNode.Content); j += 2 {
				capKey := valueNode.Content[j]
				capVal := valueNode.Content[j+1]
				switch capKey.Value {
				case "streaming", "tools", "structured_output", "vision", "audio", "reasoning", "usage_reporting":
					if capVal.Kind != yamlv3.ScalarNode || capVal.Tag != "!!bool" {
						return fmt.Errorf("models.definitions.%s.capabilities.%s must be a boolean at line %d", name, capKey.Value, capVal.Line)
					}
				case "context_tokens":
					if capVal.Kind != yamlv3.ScalarNode || capVal.Tag != "!!int" {
						return fmt.Errorf("models.definitions.%s.capabilities.context_tokens must be an integer at line %d", name, capVal.Line)
					}
				case "tokenizer":
					if capVal.Kind != yamlv3.ScalarNode || capVal.Tag != "!!str" {
						return fmt.Errorf("models.definitions.%s.capabilities.tokenizer must be a string at line %d", name, capVal.Line)
					}
				}
			}
		}
	}
	return nil
}

// validateLoadedModels owns every semantic model rule, regardless of whether
// a definition came from the file or from environment overrides.
// Definitions are independent of each other, so every one is reported.
// First-error-wins told an operator about one broken definition at a time,
// and which one depended on map iteration order.
func validateLoadedModels(models Models, routingExplicit bool) error {
	problems := make([]error, 0, len(models.Definitions)+1)
	for _, name := range slices.Sorted(maps.Keys(models.Definitions)) {
		definition := models.Definitions[name]
		problems = append(problems, modelDefinitionProblems(name, &definition)...)
	}
	problems = append(problems, validateRouting(models, routingExplicit))
	return errors.Join(problems...)
}

func modelDefinitionProblems(name string, definition *ModelDefinition) []error {
	var problems []error
	// The protocol checks are dependent: an empty protocol is not also an
	// unsupported one.
	switch {
	case strings.TrimSpace(definition.Protocol) == "" || strings.TrimSpace(definition.Model) == "":
		problems = append(problems, &Error{Code: ErrorCodeModelProtocolInvalid, Detail: fmt.Sprintf("models.definitions.%s requires non-empty protocol and model", name)})
	case !validProtocol(definition.Protocol):
		problems = append(problems, &Error{Code: ErrorCodeModelProtocolInvalid, Detail: fmt.Sprintf("models.definitions.%s uses unsupported protocol %q", name, definition.Protocol)})
	}
	if err := ValidateBaseURL(definition.BaseURL); err != nil {
		problems = append(problems, &Error{Code: ErrorCodeModelProtocolInvalid, Detail: fmt.Sprintf("models.definitions.%s.base_url: %v", name, err)})
	}
	if definition.APIKeyEnv != "" && definition.APIKeyFile != "" {
		problems = append(problems, &Error{Code: ErrorCodeModelSecretInvalid, Detail: fmt.Sprintf("models.definitions.%s must set only one of api_key_env or api_key_file", name)})
	}
	if definition.APIKeyEnv != "" && !envNamePattern.MatchString(definition.APIKeyEnv) {
		problems = append(problems, &Error{Code: ErrorCodeModelSecretInvalid, Detail: fmt.Sprintf("models.definitions.%s.api_key_env is not a valid environment variable name", name)})
	}
	if definition.APIKeyEnv == "" && definition.APIKeyFile == "" && !IsLoopbackBaseURL(definition.BaseURL) {
		problems = append(problems, &Error{Code: ErrorCodeModelSecretInvalid, Detail: fmt.Sprintf("models.definitions.%s requires api_key_env or api_key_file for a non-loopback endpoint", name)})
	}
	if definition.Capabilities.ContextTokens <= 0 || strings.TrimSpace(definition.Capabilities.Tokenizer) == "" {
		problems = append(problems, &Error{Code: ErrorCodeModelCapabilityUnsupported, Detail: fmt.Sprintf("models.definitions.%s requires positive context_tokens and a tokenizer", name)})
	}
	return problems
}

// routingExplicitIn reports whether the document declares a models.routing
// section, so the semantic pass can restrict reference checks to routing the
// user actually configured.
func routingExplicitIn(data []byte) bool {
	doc, err := parseDocument(data)
	if err != nil {
		return false
	}
	if modelsNode := mappingValue(doc, "models"); modelsNode != nil {
		return mappingValue(modelsNode, "routing") != nil
	}
	return false
}

// validateRouting checks routing roles and, when the file declares an
// explicit routing section, that each referenced role exists as a defined
// model. Default-merged routing is exempt from the reference check, since a
// single-model config legitimately routes tasks it never runs.
func validateRouting(models Models, explicit bool) error {
	for task, role := range models.Routing {
		if role != "primary" && role != "auxiliary" {
			return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("models.routing.%s: role %q is not supported (must be primary or auxiliary)", task, role)}
		}
		if explicit && len(models.Definitions) > 0 {
			if _, ok := models.Definitions[role]; !ok {
				return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("models.routing.%s references undefined model %q", task, role)}
			}
		}
	}
	return nil
}

func malformedYAMLError(err error) error {
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

func validConfigPath(path string, valid, structMapPaths map[string]bool) bool {
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
			WeaklyTypedInput: false,
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				stringToDurationHook(),
				stringToByteSizeHook(),
				stringToBoolHook(),
				stringToIntHook(),
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
	return func(from, to reflect.Type, data any) (any, error) {
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

// stringToByteSizeHook converts environment-provided size strings ("5GiB")
// without loosening file decoding.
func stringToByteSizeHook() mapstructure.DecodeHookFunc {
	return func(from, to reflect.Type, data any) (any, error) {
		if to != reflect.TypeOf(ByteSize(0)) {
			return data, nil
		}
		s, ok := data.(string)
		if !ok {
			return data, nil
		}
		b, err := parseByteSize(s)
		if err != nil {
			return nil, fmt.Errorf("invalid size %q", s)
		}
		return b, nil
	}
}

// stringToBoolHook and stringToIntHook convert environment-provided values,
// which are always strings, without loosening file decoding.
func stringToBoolHook() mapstructure.DecodeHookFunc {
	return func(from, to reflect.Type, data any) (any, error) {
		if from.Kind() != reflect.String || to.Kind() != reflect.Bool {
			return data, nil
		}
		s, ok := data.(string)
		if !ok {
			return data, nil
		}
		b, err := strconv.ParseBool(s)
		if err != nil {
			return nil, fmt.Errorf("invalid boolean %q", data)
		}
		return b, nil
	}
}

func stringToIntHook() mapstructure.DecodeHookFunc {
	return func(from, to reflect.Type, data any) (any, error) {
		if from.Kind() != reflect.String || (to.Kind() < reflect.Int || to.Kind() > reflect.Int64) {
			return data, nil
		}
		s, ok := data.(string)
		if !ok {
			return data, nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid integer %q", data)
		}
		return reflect.ValueOf(n).Convert(to).Interface(), nil
	}
}

func stringToStringSliceHook() mapstructure.DecodeHookFunc {
	return func(from, to reflect.Type, data any) (any, error) {
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
	applyToolDefaults(cfg, doc)
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
	if cfg.Models.Definitions == nil {
		cfg.Models.Definitions = defaults.Models.Definitions
	}
	if cfg.Models.RequestTimeout == 0 && !configValuePresent(doc, "models", "request_timeout") && !envValuePresent("models.request_timeout") {
		cfg.Models.RequestTimeout = defaults.Models.RequestTimeout
	}
	if cfg.Models.StreamingIdleTimeout == 0 && !configValuePresent(doc, "models", "streaming_idle_timeout") && !envValuePresent("models.streaming_idle_timeout") {
		cfg.Models.StreamingIdleTimeout = defaults.Models.StreamingIdleTimeout
	}
	if cfg.Models.Routing == nil {
		cfg.Models.Routing = defaults.Models.Routing
	}
	if cfg.Storage.BusyTimeout == 0 && !configValuePresent(doc, "storage", "busy_timeout") && !envValuePresent("storage.busy_timeout") {
		cfg.Storage.BusyTimeout = defaults.Storage.BusyTimeout
	}
	if cfg.Storage.MaxOpenConnections == 0 && !configValuePresent(doc, "storage", "max_open_connections") && !envValuePresent("storage.max_open_connections") {
		cfg.Storage.MaxOpenConnections = defaults.Storage.MaxOpenConnections
	}
	if cfg.Storage.ArtifactQuota == 0 && !configValuePresent(doc, "storage", "artifact_quota") && !envValuePresent("storage.artifact_quota") {
		cfg.Storage.ArtifactQuota = defaults.Storage.ArtifactQuota
	}
	if cfg.Storage.BackupInterval == 0 && !configValuePresent(doc, "storage", "backup_interval") && !envValuePresent("storage.backup_interval") {
		cfg.Storage.BackupInterval = defaults.Storage.BackupInterval
	}
	if cfg.Storage.BackupRetention == 0 && !configValuePresent(doc, "storage", "backup_retention") && !envValuePresent("storage.backup_retention") {
		cfg.Storage.BackupRetention = defaults.Storage.BackupRetention
	}
	if cfg.Telemetry.Exporter == "" {
		cfg.Telemetry.Exporter = defaults.Telemetry.Exporter
	}
	if cfg.Telemetry.SampleRatio == 0 && !configValuePresent(doc, "telemetry", "sample_ratio") && !envValuePresent("telemetry.sample_ratio") {
		cfg.Telemetry.SampleRatio = defaults.Telemetry.SampleRatio
	}
	if cfg.Telemetry.QueueSize == 0 && !configValuePresent(doc, "telemetry", "queue_size") && !envValuePresent("telemetry.queue_size") {
		cfg.Telemetry.QueueSize = defaults.Telemetry.QueueSize
	}
	if cfg.Telemetry.ExportTimeout == 0 && !configValuePresent(doc, "telemetry", "export_timeout") && !envValuePresent("telemetry.export_timeout") {
		cfg.Telemetry.ExportTimeout = defaults.Telemetry.ExportTimeout
	}
	if cfg.Usage.Currency == "" {
		cfg.Usage.Currency = defaults.Usage.Currency
	}
	if cfg.Usage.DailyBudgetMicros == 0 && !configValuePresent(doc, "usage", "daily_budget_micros") && !envValuePresent("usage.daily_budget_micros") {
		cfg.Usage.DailyBudgetMicros = defaults.Usage.DailyBudgetMicros
	}
	if cfg.Usage.MonthlyBudgetMicros == 0 && !configValuePresent(doc, "usage", "monthly_budget_micros") && !envValuePresent("usage.monthly_budget_micros") {
		cfg.Usage.MonthlyBudgetMicros = defaults.Usage.MonthlyBudgetMicros
	}
	if cfg.Usage.ReservationTTL == 0 && !configValuePresent(doc, "usage", "reservation_ttl") && !envValuePresent("usage.reservation_ttl") {
		cfg.Usage.ReservationTTL = defaults.Usage.ReservationTTL
	}
	applyHealthDefaults(cfg, doc, defaults.Health)
	return nil
}

func applyHealthDefaults(cfg *Config, doc *yamlv3.Node, defaults Health) {
	if cfg.Health.Listen == "" && !configValuePresent(doc, "health", "listen") && !envValuePresent("health.listen") {
		cfg.Health.Listen = defaults.Listen
	}
	if cfg.Health.CheckInterval == 0 && !configValuePresent(doc, "health", "check_interval") && !envValuePresent("health.check_interval") {
		cfg.Health.CheckInterval = defaults.CheckInterval
	}
	if cfg.Health.CheckTimeout == 0 && !configValuePresent(doc, "health", "check_timeout") && !envValuePresent("health.check_timeout") {
		cfg.Health.CheckTimeout = defaults.CheckTimeout
	}
	if cfg.Health.DiskWarningPercent == 0 && !configValuePresent(doc, "health", "disk_warning_percent") && !envValuePresent("health.disk_warning_percent") {
		cfg.Health.DiskWarningPercent = defaults.DiskWarningPercent
	}
	if cfg.Health.DiskCriticalPercent == 0 && !configValuePresent(doc, "health", "disk_critical_percent") && !envValuePresent("health.disk_critical_percent") {
		cfg.Health.DiskCriticalPercent = defaults.DiskCriticalPercent
	}
	if cfg.Health.DiskCriticalFloorBytes == 0 && !configValuePresent(doc, "health", "disk_critical_floor_bytes") && !envValuePresent("health.disk_critical_floor_bytes") {
		cfg.Health.DiskCriticalFloorBytes = defaults.DiskCriticalFloorBytes
	}
	if cfg.Health.BackupMaxAge == 0 && !configValuePresent(doc, "health", "backup_max_age") && !envValuePresent("health.backup_max_age") {
		cfg.Health.BackupMaxAge = defaults.BackupMaxAge
	}
	if cfg.Health.RestoreVerificationMaxAge == 0 && !configValuePresent(doc, "health", "restore_verification_max_age") && !envValuePresent("health.restore_verification_max_age") {
		cfg.Health.RestoreVerificationMaxAge = defaults.RestoreVerificationMaxAge
	}
}

func applyToolDefaults(cfg *Config, doc *yamlv3.Node) {
	if cfg.Tools == nil {
		return
	}
	defaults := Default().Tools
	if cfg.Tools.Workspace == "" && !configValuePresent(doc, "tools", "workspace") && !envValuePresent("tools.workspace") {
		cfg.Tools.Workspace = defaults.Workspace
	}
	if cfg.Tools.MaxInlineResultBytes == 0 && !configValuePresent(doc, "tools", "max_inline_result_bytes") && !envValuePresent("tools.max_inline_result_bytes") {
		cfg.Tools.MaxInlineResultBytes = defaults.MaxInlineResultBytes
	}
	if cfg.Tools.Exec.Timeout == 0 && !configValuePresent(doc, "tools", "exec", "timeout") && !envValuePresent("tools.exec.timeout") {
		cfg.Tools.Exec.Timeout = defaults.Exec.Timeout
	}
	if cfg.Tools.Exec.MaxStdoutBytes == 0 && !configValuePresent(doc, "tools", "exec", "max_stdout_bytes") && !envValuePresent("tools.exec.max_stdout_bytes") {
		cfg.Tools.Exec.MaxStdoutBytes = defaults.Exec.MaxStdoutBytes
	}
	if cfg.Tools.Exec.MaxStderrBytes == 0 && !configValuePresent(doc, "tools", "exec", "max_stderr_bytes") && !envValuePresent("tools.exec.max_stderr_bytes") {
		cfg.Tools.Exec.MaxStderrBytes = defaults.Exec.MaxStderrBytes
	}
	if cfg.Tools.Fetch.Timeout == 0 && !configValuePresent(doc, "tools", "fetch", "timeout") && !envValuePresent("tools.fetch.timeout") {
		cfg.Tools.Fetch.Timeout = defaults.Fetch.Timeout
	}
	if cfg.Tools.Fetch.MaxRedirects == 0 && !configValuePresent(doc, "tools", "fetch", "max_redirects") && !envValuePresent("tools.fetch.max_redirects") {
		cfg.Tools.Fetch.MaxRedirects = defaults.Fetch.MaxRedirects
	}
	if cfg.Tools.Fetch.MaxEncodedBytes == 0 && !configValuePresent(doc, "tools", "fetch", "max_encoded_bytes") && !envValuePresent("tools.fetch.max_encoded_bytes") {
		cfg.Tools.Fetch.MaxEncodedBytes = defaults.Fetch.MaxEncodedBytes
	}
	if cfg.Tools.Fetch.MaxDecodedBytes == 0 && !configValuePresent(doc, "tools", "fetch", "max_decoded_bytes") && !envValuePresent("tools.fetch.max_decoded_bytes") {
		cfg.Tools.Fetch.MaxDecodedBytes = defaults.Fetch.MaxDecodedBytes
	}
	if cfg.Tools.WebSearch.Provider == "" && !configValuePresent(doc, "tools", "web_search", "provider") && !envValuePresent("tools.web_search.provider") {
		cfg.Tools.WebSearch.Provider = defaults.WebSearch.Provider
	}
	if cfg.Tools.WebSearch.CredentialRef == "" && !configValuePresent(doc, "tools", "web_search", "credential_ref") && !envValuePresent("tools.web_search.credential_ref") {
		cfg.Tools.WebSearch.CredentialRef = defaults.WebSearch.CredentialRef
	}
	if cfg.Tools.WebSearch.MaxResults == 0 && !configValuePresent(doc, "tools", "web_search", "max_results") && !envValuePresent("tools.web_search.max_results") {
		cfg.Tools.WebSearch.MaxResults = defaults.WebSearch.MaxResults
	}
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
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "runtime.max_active_turns must be positive"}
	}
	if runtime.MaxPendingTurns < runtime.MaxActiveTurns {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "runtime.max_pending_turns must be at least runtime.max_active_turns"}
	}
	if runtime.TurnTimeout <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "runtime.turn_timeout must be positive"}
	}
	if runtime.ShutdownTimeout <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "runtime.shutdown_timeout must be positive"}
	}
	return nil
}

func validateServer(server Server) error {
	if server.Port < 1 || server.Port > 65535 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("server.port %d is out of range (1-65535)", server.Port)}
	}
	return nil
}

func validateLogging(loggingConfig Logging) error {
	if _, err := logging.ParseLevel(loggingConfig.Level); err != nil {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: err.Error()}
	}
	if _, err := logging.ParseFormat(loggingConfig.Format); err != nil {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: err.Error()}
	}
	return nil
}

func validateStorage(storage Storage) error {
	if storage.MaxOpenConnections < 1 || storage.MaxOpenConnections > 16 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("storage.max_open_connections %d is out of range (1-16)", storage.MaxOpenConnections)}
	}
	if storage.BusyTimeout <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "storage.busy_timeout must be positive"}
	}
	if storage.ArtifactQuota <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "storage.artifact_quota must be positive"}
	}
	if storage.BackupInterval <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "storage.backup_interval must be positive"}
	}
	if storage.BackupRetention < 1 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "storage.backup_retention must be at least 1"}
	}
	return nil
}

func validateTelemetry(t Telemetry) error {
	switch t.Exporter {
	case "", "none", "stdout", "otlp_grpc", "otlp_http":
	default:
		return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("telemetry.exporter %q is not supported (none, stdout, otlp_grpc, otlp_http)", t.Exporter)}
	}
	if t.SampleRatio < 0 || t.SampleRatio > 1 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("telemetry.sample_ratio %f is out of range (0-1)", t.SampleRatio)}
	}
	if t.QueueSize < 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "telemetry.queue_size must not be negative"}
	}
	if t.ExportTimeout < 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "telemetry.export_timeout must not be negative"}
	}
	if (t.Exporter == "otlp_grpc" || t.Exporter == "otlp_http") && t.Endpoint == "" {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("telemetry.endpoint is required for exporter %q", t.Exporter)}
	}
	return nil
}

func validateUsage(u Usage) error {
	if u.Currency == "" {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "usage.currency must not be empty"}
	}
	if u.DailyBudgetMicros < 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "usage.daily_budget_micros must not be negative"}
	}
	if u.MonthlyBudgetMicros < 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "usage.monthly_budget_micros must not be negative"}
	}
	if u.ReservationTTL <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "usage.reservation_ttl must be positive"}
	}
	return nil
}

func validateHealth(h Health) error {
	if err := validateLoopbackListen(h.Listen, "health.listen"); err != nil {
		return err
	}
	if h.CheckInterval <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "health.check_interval must be positive"}
	}
	if h.CheckTimeout <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "health.check_timeout must be positive"}
	}
	if h.CheckTimeout > h.CheckInterval {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "health.check_timeout must not exceed health.check_interval"}
	}
	if h.DiskWarningPercent < 1 || h.DiskWarningPercent > 99 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "health.disk_warning_percent must be between 1 and 99"}
	}
	if h.DiskCriticalPercent < 1 || h.DiskCriticalPercent > 99 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "health.disk_critical_percent must be between 1 and 99"}
	}
	if h.DiskCriticalPercent >= h.DiskWarningPercent {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "health.disk_critical_percent must be below health.disk_warning_percent"}
	}
	if h.DiskCriticalFloorBytes <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "health.disk_critical_floor_bytes must be positive"}
	}
	if h.BackupMaxAge <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "health.backup_max_age must be positive"}
	}
	if h.RestoreVerificationMaxAge <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "health.restore_verification_max_age must be positive"}
	}
	return nil
}

// validateLoopbackListen enforces the probe exposure boundary: host:port with
// a loopback host. Non-loopback probe exposure needs the authenticated admin
// surface, not this listener.
func validateLoopbackListen(listen, field string) error {
	host, portText, err := net.SplitHostPort(listen)
	if err != nil {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("%s must be host:port: %v", field, err)}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: field + " port must be between 1 and 65535"}
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return &Error{Code: ErrorCodeConfigInvalid, Detail: field + " must bind loopback; non-loopback probe exposure requires the authenticated admin surface"}
}

func validateTools(toolsConfig *Tools, profile capability.Profile) error {
	if toolsConfig == nil {
		if profile != capability.ProfileCore {
			return &Error{Code: ErrorCodeConfigInvalid, Detail: fmt.Sprintf("tools section is required for build profile %q", profile)}
		}
		return nil
	}
	if strings.TrimSpace(toolsConfig.Workspace) == "" {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "tools.workspace must not be empty"}
	}
	if !filepath.IsAbs(toolsConfig.Workspace) {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "tools.workspace must be an absolute path"}
	}
	if toolsConfig.MaxInlineResultBytes <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "tools.max_inline_result_bytes must be positive"}
	}
	if toolsConfig.Exec.Timeout <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "tools.exec.timeout must be positive"}
	}
	if toolsConfig.Exec.MaxStdoutBytes <= 0 || toolsConfig.Exec.MaxStderrBytes <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "tools.exec output limits must be positive"}
	}
	if toolsConfig.Fetch.Timeout <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "tools.fetch.timeout must be positive"}
	}
	if toolsConfig.Fetch.MaxRedirects < 0 || toolsConfig.Fetch.MaxRedirects > 10 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "tools.fetch.max_redirects must be between 0 and 10"}
	}
	if toolsConfig.Fetch.MaxEncodedBytes <= 0 || toolsConfig.Fetch.MaxDecodedBytes <= 0 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "tools.fetch body limits must be positive"}
	}
	if strings.TrimSpace(toolsConfig.WebSearch.Provider) == "" {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "tools.web_search.provider must not be empty"}
	}
	if !strings.HasPrefix(toolsConfig.WebSearch.CredentialRef, "env://") && !strings.HasPrefix(toolsConfig.WebSearch.CredentialRef, "file://") {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "tools.web_search.credential_ref must use env:// or file://"}
	}
	if toolsConfig.WebSearch.MaxResults < 1 || toolsConfig.WebSearch.MaxResults > 20 {
		return &Error{Code: ErrorCodeConfigInvalid, Detail: "tools.web_search.max_results must be between 1 and 20"}
	}
	return nil
}

// unknownEnvKeys lists AURA_-prefixed environment variables that no config
// path or model definition maps to, so misconfigured overrides are surfaced
// instead of silently ignored. AURA_CONFIG is a loader input, not a value.
func unknownEnvKeys() []string {
	warnings := make([]string, 0, len(os.Environ()))
	known := make(map[string]bool, len(envLookup))
	for key := range envLookup {
		known[key] = true
	}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, envPrefix) {
			continue
		}
		key, _, _ := strings.Cut(kv, "=")
		if key == envConfigVar {
			continue
		}
		normalized := strings.ToLower(strings.TrimPrefix(key, envPrefix))
		if known[normalized] {
			continue
		}
		if strings.HasPrefix(normalized, "models_definitions_") && envKeyMapper(key) != "" {
			continue
		}
		warnings = append(warnings, key)
	}
	return warnings
}

func checkVersion(v int) error {
	if v != supportedVersion {
		return &Error{
			Code:   ErrorCodeVersionUnsupported,
			Detail: fmt.Sprintf("unsupported version %d (supported: %d)", v, supportedVersion),
		}
	}
	return nil
}

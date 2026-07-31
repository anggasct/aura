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
	res, err := load(path)
	if err != nil {
		return LoadResult{}, fmt.Errorf("config: %w", err)
	}
	return res, nil
}

func load(path string) (LoadResult, error) {
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
	return LoadResult{Config: cfg, Path: resolved, DefaultGenerated: generated}, nil
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
	var root yamlv3.Node
	if err := yamlv3.Unmarshal(data, &root); err != nil {
		return malformedYAMLError(err)
	}
	if root.Kind != yamlv3.DocumentNode || len(root.Content) == 0 {
		return fmt.Errorf("invalid YAML: empty document")
	}
	doc := root.Content[0]
	if doc.Kind != yamlv3.MappingNode {
		return fmt.Errorf("invalid YAML: top level must be a mapping")
	}
	valid, mapPaths := validKeyPaths()
	return checkUnknownKeys(doc, "", valid, mapPaths)
}

func malformedYAMLError(err error) error {
	if m := yamlLineRe.FindStringSubmatch(err.Error()); len(m) == 2 {
		line, _ := strconv.Atoi(m[1])
		return fmt.Errorf("invalid YAML at line %d: %w", line, err)
	}
	return fmt.Errorf("invalid YAML: %w", err)
}

func checkUnknownKeys(node *yamlv3.Node, prefix string, valid, mapPaths map[string]bool) error {
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		path := keyNode.Value
		if prefix != "" {
			path = prefix + "." + path
		}
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
			DecodeHook:       mapstructure.ComposeDecodeHookFunc(stringToDurationHook()),
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

func checkVersion(v int) error {
	if v != supportedVersion {
		return fmt.Errorf("unsupported version %q (supported: %d)", strconv.Itoa(v), supportedVersion)
	}
	return nil
}

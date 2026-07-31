package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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
	supportedVersion = 1
)

var (
	envLookup  = buildEnvLookup()
	yamlLineRe = regexp.MustCompile(`line (\d+)`)
)

func buildEnvLookup() map[string]string {
	m := map[string]string{}
	for path := range validKeyPaths() {
		m[strings.ReplaceAll(path, ".", "_")] = path
	}
	return m
}

func envKeyMapper(s string) string {
	return envLookup[strings.ToLower(strings.TrimPrefix(s, envPrefix))]
}

// Load reads configuration from path, or the default XDG location when path is
// empty. A missing default config is auto-generated; a missing explicit path is
// an error. AURA_-prefixed environment variables override file values.
func Load(path string) (*Config, error) {
	resolved, explicit, err := resolvePath(path)
	if err != nil {
		return nil, err
	}
	data, err := loadBytes(resolved, explicit)
	if err != nil {
		return nil, err
	}
	if err := validate(data); err != nil {
		return nil, err
	}
	cfg, err := decode(data)
	if err != nil {
		return nil, err
	}
	if err := checkVersion(cfg.Version); err != nil {
		return nil, err
	}
	return cfg, nil
}

func resolvePath(explicit string) (string, bool, error) {
	if explicit != "" {
		return explicit, true, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", false, fmt.Errorf("cannot resolve config directory: set HOME or XDG_CONFIG_HOME: %w", err)
	}
	return filepath.Join(base, "aura", "config.yaml"), false, nil
}

func loadBytes(path string, explicit bool) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if explicit {
				return nil, fmt.Errorf("file not found: %s", path)
			}
			return writeDefault(path)
		}
		if errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("cannot read %s: permission denied", path)
		}
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("cannot read %s: permission denied", path)
		}
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}
	return data, nil
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
	slog.Info("generating default config", "component", "config", "path", path)
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
	return checkUnknownKeys(doc, "", validKeyPaths())
}

func malformedYAMLError(err error) error {
	if m := yamlLineRe.FindStringSubmatch(err.Error()); len(m) == 2 {
		line, _ := strconv.Atoi(m[1])
		return fmt.Errorf("invalid YAML at line %d: %w", line, err)
	}
	return fmt.Errorf("invalid YAML: %w", err)
}

func checkUnknownKeys(node *yamlv3.Node, prefix string, valid map[string]bool) error {
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
		if valNode.Kind == yamlv3.MappingNode {
			if err := checkUnknownKeys(valNode, path, valid); err != nil {
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

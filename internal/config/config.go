package config

import (
	"bytes"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Version int     `koanf:"version" yaml:"version"`
	Server  Server  `koanf:"server" yaml:"server"`
	Logging Logging `koanf:"logging" yaml:"logging"`
	Models  Models  `koanf:"models" yaml:"models"`
}

type Server struct {
	Host            string   `koanf:"host" yaml:"host"`
	Port            int      `koanf:"port" yaml:"port"`
	ShutdownTimeout Duration `koanf:"shutdown_timeout" yaml:"shutdown_timeout"`
}

type Logging struct {
	Level  string `koanf:"level" yaml:"level"`
	Format string `koanf:"format" yaml:"format"`
}

type Models struct {
	Primary              ModelSpec         `koanf:"primary" yaml:"primary"`
	Auxiliary            ModelSpec         `koanf:"auxiliary" yaml:"auxiliary"`
	RequestTimeout       Duration          `koanf:"request_timeout" yaml:"request_timeout"`
	StreamingIdleTimeout Duration          `koanf:"streaming_idle_timeout" yaml:"streaming_idle_timeout"`
	FallbackNotify       bool              `koanf:"fallback_notify" yaml:"fallback_notify"`
	CircuitCooldown      Duration          `koanf:"circuit_cooldown" yaml:"circuit_cooldown"`
	CircuitThreshold     int               `koanf:"circuit_threshold" yaml:"circuit_threshold"`
	Routing              map[string]string `koanf:"routing" yaml:"routing"`
}

type ModelSpec struct {
	Provider  string   `koanf:"provider" yaml:"provider"`
	Model     string   `koanf:"model" yaml:"model"`
	APIKey    string   `koanf:"api_key" yaml:"api_key"`
	BaseURL   string   `koanf:"base_url" yaml:"base_url"`
	Fallbacks []string `koanf:"fallbacks" yaml:"fallbacks"`
}

type Duration time.Duration

func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

func Default() Config {
	return Config{
		Version: 1,
		Server: Server{
			Host:            "127.0.0.1",
			Port:            8280,
			ShutdownTimeout: Duration(30 * time.Second),
		},
		Logging: Logging{
			Level:  "info",
			Format: "text",
		},
		Models: Models{
			RequestTimeout:       Duration(120 * time.Second),
			StreamingIdleTimeout: Duration(60 * time.Second),
			CircuitCooldown:      Duration(5 * time.Minute),
			CircuitThreshold:     3,
			Routing:              defaultModelsRouting(),
		},
	}
}

func defaultModelsRouting() map[string]string {
	return map[string]string{
		"summarize":        "auxiliary",
		"vision":           "primary",
		"title_gen":        "auxiliary",
		"curator":          "auxiliary",
		"context_compress": "auxiliary",
		"profiling":        "auxiliary",
	}
}

func Marshal(c Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func validKeyPaths() (map[string]bool, map[string]bool) {
	paths := map[string]bool{}
	mapPaths := map[string]bool{}
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("koanf"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			path := tag
			if prefix != "" {
				path = prefix + "." + tag
			}
			paths[path] = true
			ft := f.Type
			if ft.Kind() == reflect.Map {
				mapPaths[path] = true
				continue
			}
			if ft.Kind() == reflect.Struct && ft != reflect.TypeOf(Duration(0)) {
				walk(ft, path)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	return paths, mapPaths
}

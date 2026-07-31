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

func validKeyPaths() map[string]bool {
	paths := map[string]bool{}
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
			if ft := f.Type; ft.Kind() == reflect.Struct && ft != reflect.TypeOf(Duration(0)) {
				walk(ft, path)
			}
		}
	}
	walk(reflect.TypeOf(Config{}), "")
	return paths
}

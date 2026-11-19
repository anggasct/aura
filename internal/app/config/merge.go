package config

import (
	"encoding/json"
	"errors"
	"reflect"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

var optionalSections = []string{
	"agents", "tools", "mcp", "plugins", "skills", "context", "profile",
	"owner", "children", "sync", "vision", "knowledge",
}

var rootType = reflect.TypeFor[Config]()

type layerProvider map[string]any

func (layerProvider) ReadBytes() ([]byte, error) {
	return nil, fault.New(configkit.ErrorCodeInvalid, "layer provider does not support this method")
}

func (p layerProvider) Read() (map[string]any, error) {
	return p, nil
}

func loadLayers(doc *document, collector *envCollector) (map[string]any, error) {
	file, err := readLayer(rawbytes.Provider(doc.data), yaml.Parser())
	if err != nil {
		return nil, malformedDocument(fault.Wrap(configkit.ErrorCodeMalformed, "invalid YAML", err))
	}
	environment, err := readLayer(env.ProviderWithValue(envPrefix, ".", collector.provide), nil)
	if err != nil {
		return nil, layerFault("failed to load environment overrides", err)
	}
	bottom, err := defaultsLayer(file, environment)
	if err != nil {
		return nil, err
	}
	merged := mergeFile(bottom, file, "")
	mergeEnvironment(merged, environment)
	var walk elementWalk
	merged = walk.fields(merged, "", rootType)
	if walk.err != nil {
		return nil, walk.err
	}
	return readLayer(layerProvider(merged), nil)
}

func readLayer(provider koanf.Provider, parser koanf.Parser) (map[string]any, error) {
	k := koanf.New(".")
	if err := k.Load(provider, parser); err != nil {
		return nil, err
	}
	return k.Raw(), nil
}

func defaultsLayer(file, environment map[string]any) (map[string]any, error) {
	layer, err := valueAsMap(Default())
	if err != nil {
		return nil, err
	}
	for _, section := range optionalSections {
		_, inFile := file[section]
		_, inEnvironment := environment[section]
		if !inFile && !inEnvironment {
			delete(layer, section)
		}
	}
	return layer, nil
}

func valueAsMap(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, layerFault("failed to encode defaults", err)
	}
	decoded, err := yaml.Parser().Unmarshal(encoded)
	if err != nil {
		return nil, layerFault("failed to read encoded defaults", err)
	}
	dropNulls(decoded)
	return decoded, nil
}

func layerFault(detail string, err error) error {
	return configkit.Field("", fault.Wrap(configkit.ErrorCodeInvalid, detail, err))
}

func dropNulls(node map[string]any) {
	for key, value := range node {
		switch typed := value.(type) {
		case nil:
			delete(node, key)
		case map[string]any:
			dropNulls(typed)
		}
	}
}

func mergeFile(base, file map[string]any, scope string) map[string]any {
	for key, value := range file {
		child := joinPath(scope, key)
		info, known := configIndex[child]
		incoming, incomingIsMap := value.(map[string]any)
		existing, existingIsMap := base[key].(map[string]any)
		if known && info.typ.Kind() == reflect.Struct && incomingIsMap && existingIsMap {
			base[key] = mergeFile(existing, incoming, child)
			continue
		}
		base[key] = value
	}
	return base
}

func mergeEnvironment(base, environment map[string]any) {
	for key, value := range environment {
		incoming, incomingIsMap := value.(map[string]any)
		existing, existingIsMap := base[key].(map[string]any)
		if incomingIsMap && existingIsMap {
			mergeEnvironment(existing, incoming)
			continue
		}
		base[key] = value
	}
}

type elementWalk struct {
	err error
}

func (w *elementWalk) value(value any, scope string, typ reflect.Type) any {
	switch typ.Kind() {
	case reflect.Struct:
		if fields, isMap := value.(map[string]any); isMap {
			return w.fields(fields, scope, typ)
		}
	case reflect.Map:
		if entries, isMap := value.(map[string]any); isMap {
			for key, entry := range entries {
				entries[key] = w.value(entry, joinPath(scope, mapEntryWildcard), dereference(typ.Elem()))
			}
		}
	case reflect.Slice:
		if items, isList := value.([]any); isList {
			for i, item := range items {
				items[i] = w.value(item, scope, dereference(typ.Elem()))
			}
		}
	default:
	}
	return value
}

func (w *elementWalk) fields(fields map[string]any, scope string, typ reflect.Type) map[string]any {
	if defaulter, defaulted := reflect.New(typ).Interface().(configkit.Defaulter); defaulted {
		layer, err := valueAsMap(defaulter.Default())
		w.err = errors.Join(w.err, err)
		fields = mergeFile(layer, fields, scope)
	}
	for key, child := range fields {
		if info, known := configIndex[joinPath(scope, key)]; known {
			fields[key] = w.value(child, joinPath(scope, key), info.typ)
		}
	}
	return fields
}

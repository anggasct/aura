package config

import (
	"encoding"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	mapEntryWildcard   = "*"
	environmentRefusal = "is not addressable from the environment"
)

var mapsWithDefaultEntries = []string{"models.routing"}

var environment = newEnvTable(configIndex)

type envOutcome int

const (
	envIgnored envOutcome = iota
	envSet
	envRefused
)

type envMatch struct {
	outcome envOutcome
	path    string
	typ     reflect.Type
}

type envPattern struct {
	path   string
	prefix string
	suffix string
}

type envTable struct {
	index    fieldIndex
	exact    map[string]string
	patterns []envPattern
}

type envProblem struct {
	name string
	err  error
}

func newEnvTable(index fieldIndex) envTable {
	table := envTable{index: index, exact: map[string]string{}}
	for path, info := range index {
		switch strings.Count(path, mapEntryWildcard) {
		case 0:
			table.exact[environmentForm(path)] = path
		case 1:
			if info.leaf {
				prefix, suffix, _ := strings.Cut(environmentForm(path), mapEntryWildcard)
				table.patterns = append(table.patterns, envPattern{path: path, prefix: prefix, suffix: suffix})
			}
		}
	}
	slices.SortFunc(table.patterns, func(a, b envPattern) int {
		if byLength := len(b.suffix) - len(a.suffix); byLength != 0 {
			return byLength
		}
		return len(b.prefix) - len(a.prefix)
	})
	return table
}

func (t envTable) resolve(name string) envMatch {
	if name == envConfigVar {
		return envMatch{}
	}
	normalized := strings.ToLower(strings.TrimPrefix(name, envPrefix))
	if path, found := t.exact[normalized]; found {
		info := t.index[path]
		if addressable(path, info) {
			return envMatch{outcome: envSet, path: path, typ: info.typ}
		}
		return envMatch{outcome: envRefused, path: path}
	}
	for _, pattern := range t.patterns {
		key, matched := pattern.key(normalized)
		if !matched {
			continue
		}
		info := t.index[pattern.path]
		if !addressable(pattern.path, info) {
			return envMatch{}
		}
		return envMatch{outcome: envSet, path: strings.Replace(pattern.path, mapEntryWildcard, key, 1), typ: info.typ}
	}
	return envMatch{}
}

func (p envPattern) key(normalized string) (string, bool) {
	if len(normalized) <= len(p.prefix)+len(p.suffix) || !strings.HasPrefix(normalized, p.prefix) || !strings.HasSuffix(normalized, p.suffix) {
		return "", false
	}
	key := normalized[len(p.prefix) : len(normalized)-len(p.suffix)]
	for _, r := range key {
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return "", false
		}
	}
	return key, true
}

func addressable(path string, info fieldInfo) bool {
	if !info.leaf || info.underList {
		return false
	}
	for _, owner := range mapsWithDefaultEntries {
		if strings.HasPrefix(path, owner+".") {
			return false
		}
	}
	return true
}

func parseEnvValue(t reflect.Type, raw string) (any, bool) {
	if _, text := textKinds[t]; text {
		target, isText := reflect.New(t).Interface().(encoding.TextUnmarshaler)
		return raw, isText && target.UnmarshalText([]byte(raw)) == nil
	}
	switch t.Kind() {
	case reflect.Bool:
		value, err := strconv.ParseBool(raw)
		return reflect.ValueOf(value).Convert(t).Interface(), err == nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value, err := strconv.ParseInt(raw, 10, t.Bits())
		return reflect.ValueOf(value).Convert(t).Interface(), err == nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value, err := strconv.ParseUint(raw, 10, t.Bits())
		return reflect.ValueOf(value).Convert(t).Interface(), err == nil
	case reflect.Float32, reflect.Float64:
		value, err := strconv.ParseFloat(raw, t.Bits())
		return reflect.ValueOf(value).Convert(t).Interface(), err == nil
	case reflect.Slice:
		return parseStringList(raw)
	default:
		return raw, true
	}
}

func parseStringList(raw string) ([]string, bool) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, true
	}
	items := strings.Split(raw, ",")
	for i := range items {
		items[i] = strings.TrimSpace(items[i])
		if items[i] == "" {
			return nil, false
		}
	}
	return items, true
}

func envProblemAt(name, path, detail string) envProblem {
	return envProblem{name: name, err: &configkit.FieldError{
		Path:     path,
		Position: "env " + name,
		Err:      fault.New(configkit.ErrorCodeInvalid, detail),
	}}
}

type envCollector struct {
	problems []envProblem
}

func (c *envCollector) provide(name, raw string) (key string, value any) {
	match := environment.resolve(name)
	switch match.outcome {
	case envSet:
		parsedValue, parsed := parseEnvValue(match.typ, raw)
		if parsed {
			return match.path, parsedValue
		}
		kind, _ := leafKind(match.typ)
		c.problems = append(c.problems, envProblemAt(name, match.path, "must be "+kind))
	case envRefused:
		c.problems = append(c.problems, envProblemAt(name, match.path, environmentRefusal))
	case envIgnored:
	}
	return "", nil
}

func (c *envCollector) errors() []error {
	slices.SortFunc(c.problems, func(a, b envProblem) int {
		return strings.Compare(a.name, b.name)
	})
	errs := make([]error, len(c.problems))
	for i, problem := range c.problems {
		errs[i] = problem.err
	}
	return errs
}

func environmentNames() []string {
	environ := os.Environ()
	names := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, envPrefix) {
			names = append(names, name)
		}
	}
	return names
}

func envValuePresent(path string) bool {
	for _, name := range environmentNames() {
		if match := environment.resolve(name); match.outcome == envSet && match.path == path {
			return true
		}
	}
	return false
}

func envWarnings(cfg *Config) []string {
	configured := configuredEnvNames(cfg)
	names := environmentNames()
	warnings := make([]string, 0, len(names))
	for _, name := range names {
		if name == envConfigVar || configured[name] || environment.resolve(name).outcome != envIgnored {
			continue
		}
		warnings = append(warnings, name)
	}
	return warnings
}

func configuredEnvNames(cfg *Config) map[string]bool {
	names := map[string]bool{}
	collectEnvNames(reflect.ValueOf(cfg), "", names)
	return names
}

func collectEnvNames(v reflect.Value, tag string, names map[string]bool) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			collectEnvNames(v.Elem(), tag, names)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("json"), ",")
			if name != "" && name != "-" {
				collectEnvNames(v.Field(i), name, names)
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			collectEnvNames(v.Index(i), tag, names)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			collectEnvNames(v.MapIndex(key), tag, names)
		}
	case reflect.String:
		if name := referencedEnvName(tag, v.String()); name != "" {
			names[name] = true
		}
	default:
	}
}

func referencedEnvName(tag, value string) string {
	switch {
	case strings.HasSuffix(tag, "_env"):
		return value
	case tag == "ref" || strings.HasSuffix(tag, "_ref"):
		name, _ := strings.CutPrefix(value, "env://")
		if name == value {
			return ""
		}
		return name
	}
	return ""
}

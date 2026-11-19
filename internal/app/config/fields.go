package config

import (
	"encoding"
	"errors"
	"reflect"
	"strings"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/secret"
)

type fieldInfo struct {
	typ       reflect.Type
	leaf      bool
	underList bool
}

type fieldIndex map[string]fieldInfo

var configIndex = mustBuildIndex(reflect.TypeFor[Config]())

var textKinds = map[reflect.Type]string{
	reflect.TypeFor[configkit.Duration](): "a duration",
	reflect.TypeFor[configkit.ByteSize](): "a byte size",
	reflect.TypeFor[secret.Ref]():         "a secret reference",
}

var textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()

func mustBuildIndex(t reflect.Type) fieldIndex {
	index, err := buildIndex(t)
	if err != nil {
		panic(err)
	}
	return index
}

func buildIndex(t reflect.Type) (fieldIndex, error) {
	b := &indexBuilder{index: fieldIndex{}, owners: map[string]string{}}
	b.addFields(dereference(t), "", false)
	return b.index, errors.Join(b.errs...)
}

type indexBuilder struct {
	index  fieldIndex
	owners map[string]string
	errs   []error
}

func (b *indexBuilder) addFields(t reflect.Type, prefix string, underList bool) {
	if t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		field := t.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		b.add(field.Type, path, underList)
	}
}

func (b *indexBuilder) add(t reflect.Type, path string, underList bool) {
	t = dereference(t)
	_, leaf := leafKind(t)
	b.record(path, fieldInfo{typ: t, leaf: leaf, underList: underList})
	if leaf {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		b.addFields(t, path, underList)
	case reflect.Map:
		b.add(t.Elem(), path+".*", underList)
	case reflect.Slice:
		b.addFields(dereference(t.Elem()), path, true)
	default:
	}
}

func (b *indexBuilder) record(path string, info fieldInfo) {
	form := environmentForm(path)
	if owner, taken := b.owners[form]; taken {
		b.errs = append(b.errs, configkit.Invalid(owner, "shares the environment name "+form+" with "+path))
	}
	b.owners[form] = path
	b.index[path] = info
}

func environmentForm(path string) string {
	return strings.ReplaceAll(path, ".", "_")
}

func dereference(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Pointer {
		return t.Elem()
	}
	return t
}

func leafKind(t reflect.Type) (string, bool) {
	if kind, text := textKinds[t]; text {
		return kind, true
	}
	if reflect.PointerTo(t).Implements(textUnmarshalerType) {
		return "", false
	}
	switch t.Kind() {
	case reflect.Bool:
		return "a boolean", true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer", true
	case reflect.Float32, reflect.Float64:
		return "a number", true
	case reflect.String:
		return "", true
	case reflect.Slice:
		if t.Elem().Kind() == reflect.String {
			return "a list of strings without empty items", true
		}
	default:
	}
	return "", false
}

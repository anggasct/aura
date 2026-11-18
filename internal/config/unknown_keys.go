package config

import (
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

type unknownKeyWalk struct {
	doc      *document
	problems []error
}

type keyScope struct {
	index string
	shown string
	typ   reflect.Type
}

func reportUnknownKeys(doc *document, input map[string]any) []error {
	walk := unknownKeyWalk{doc: doc}
	walk.fields(input, keyScope{})
	return walk.problems
}

func (w *unknownKeyWalk) fields(input map[string]any, scope keyScope) {
	for _, key := range slices.Sorted(maps.Keys(input)) {
		value := input[key]
		child := keyScope{index: joinPath(scope.index, key), shown: joinPath(scope.shown, key)}
		info, known := configIndex[child.index]
		if !known || strings.Contains(key, ".") {
			w.problems = append(w.problems, &configkit.FieldError{
				Path:     child.shown,
				Position: w.doc.keyPositionOf(child.shown),
				Err:      fault.New(configkit.ErrorCodeUnknownKey, "unknown key"),
			})
			delete(input, key)
			continue
		}
		child.typ = info.typ
		w.value(value, child)
	}
}

func (w *unknownKeyWalk) value(value any, scope keyScope) {
	switch scope.typ.Kind() {
	case reflect.Struct:
		if entries, isMap := value.(map[string]any); isMap {
			w.fields(entries, scope)
		}
	case reflect.Map:
		entries, isMap := value.(map[string]any)
		element, indexed := configIndex[joinPath(scope.index, mapEntryWildcard)]
		if !isMap || !indexed {
			return
		}
		for _, key := range slices.Sorted(maps.Keys(entries)) {
			entry := entries[key]
			w.value(entry, keyScope{index: joinPath(scope.index, mapEntryWildcard), shown: joinPath(scope.shown, key), typ: element.typ})
		}
	case reflect.Slice:
		items, isList := value.([]any)
		if !isList {
			return
		}
		for i, item := range items {
			w.value(item, keyScope{index: scope.index, shown: scope.shown + "[" + strconv.Itoa(i) + "]", typ: dereference(scope.typ.Elem())})
		}
	default:
	}
}

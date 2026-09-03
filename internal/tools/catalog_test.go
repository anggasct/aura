package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The builtin definition registry in this package is the only home for tool
// definition data; the harness conformance suite must not carry its own copy
// of names, versions, or schemas.
func TestBuiltinDefinitionsHaveNoDuplicateRegistryInHarness(t *testing.T) {
	builtins := Builtins()
	keys := make([]string, 0, len(builtins))
	for _, definition := range builtins {
		keys = append(keys, definition.Key())
	}
	forbidden := append([]string{
		"\"github.com/anggasct/aura/internal/tools\"",
		"\"github.com/anggasct/aura/internal/tools/exec\"",
		"\"github.com/anggasct/aura/internal/tools/fetch\"",
		"\"github.com/anggasct/aura/internal/tools/filesystem\"",
		"\"github.com/anggasct/aura/internal/tools/search\"",
		"aura://tools/",
	}, builtinKeyLiterals(keys)...)
	err := filepath.WalkDir(filepath.Join("..", "harness"), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source := string(content)
		for _, forbidden := range forbidden {
			if strings.Contains(source, forbidden) {
				t.Errorf("%s duplicates builtin tool definition data (%s); this package's registry is the single source", path, strings.Trim(forbidden, "\""))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan internal/harness: %v", err)
	}
}

func TestDefinitionsByKeyMatchesBuiltins(t *testing.T) {
	byKey := DefinitionsByKey()
	builtins := Builtins()
	if len(byKey) != len(builtins) {
		t.Fatalf("DefinitionsByKey = %d entries, want %d", len(byKey), len(builtins))
	}
	for _, definition := range builtins {
		registered, ok := byKey[definition.Key()]
		if !ok {
			t.Errorf("definition %q is missing from DefinitionsByKey", definition.Key())
			continue
		}
		if registered.Name != definition.Name || registered.Version != definition.Version {
			t.Errorf("definition %q drifted between Builtins and DefinitionsByKey", definition.Key())
		}
	}
}

func builtinKeyLiterals(keys []string) []string {
	literals := make([]string, 0, len(keys))
	for _, key := range keys {
		literals = append(literals, "\""+key+"\"")
	}
	return literals
}

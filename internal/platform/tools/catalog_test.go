package tools

import "testing"

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

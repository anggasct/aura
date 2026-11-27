package agent

import (
	"testing"
)

func TestRegistryPersonasMessage(t *testing.T) {
	override := kantorOverride()
	override.Messages = map[string]string{MessageTurnFailed: "Maaf, ada masalah."}
	registry, err := Build([]Persona{override}, validTools, validToolCatalog, []string{"primary"}, "en")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cases := []struct {
		name     string
		personas RegistryPersonas
		agentID  string
		key      string
		want     string
	}{
		{name: "a persona override", personas: RegistryPersonas{Registry: registry}, agentID: "kantor", key: MessageTurnFailed, want: "Maaf, ada masalah."},
		{name: "a persona without an override", personas: RegistryPersonas{Registry: registry}, agentID: "kantor", key: MessageTurnTimeout, want: PersonaMessageDefaults()[MessageTurnTimeout]},
		{name: "an unknown agent", personas: RegistryPersonas{Registry: registry}, agentID: "ghost", key: MessageTurnEmpty, want: PersonaMessageDefaults()[MessageTurnEmpty]},
		{name: "a missing registry", personas: RegistryPersonas{}, agentID: "kantor", key: MessageTurnFailed, want: PersonaMessageDefaults()[MessageTurnFailed]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.personas.Message(tc.agentID, tc.key); got != tc.want {
				t.Fatalf("Message(%q, %q) = %q, want %q", tc.agentID, tc.key, got, tc.want)
			}
		})
	}
}

func TestTurnMessageDefaultsPassTheMessageRules(t *testing.T) {
	for _, key := range []string{MessageTurnFailed, MessageTurnTimeout, MessageTurnEmpty} {
		t.Run(key, func(t *testing.T) {
			defaults := PersonaMessageDefaults()
			if err := validateMessages("definitions[0]", map[string]string{key: defaults[key]}); err != nil {
				t.Fatalf("default of %s is refused: %v", key, err)
			}
		})
	}
}

package config

import (
	"slices"
	"strings"
	"testing"

	legacyconfig "github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/secret"
)

func TestReferencesAreValidatedOnceAndJoined(t *testing.T) {
	const canaryEnv = "AURA_TEST_REFERENCES_CANARY"
	const canary = "canary-secret-for-reference-screening"
	t.Setenv(canaryEnv, canary)
	ref, err := secret.ParseRef("env://" + canaryEnv)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	cfg := Default()
	cfg.Telemetry.CredentialRef = ref
	cfg.Models.Definitions = map[string]legacyconfig.ModelDefinition{"defined": {}}
	cfg.ModelRoutes = map[string]legacyconfig.ModelRoute{"fast": {Candidates: []string{"undefined-definition"}}}
	cfg.Bindings = []legacyconfig.Binding{{Persona: "default", Channel: legacyconfig.BindingChannelDiscord, Account: "undefined-account", GuildID: "123"}}
	cfg.Owner = &legacyconfig.Owner{Name: "owner " + canary}
	doc, err := parseDocument([]byte("version: 1\n"))
	if err != nil {
		t.Fatalf("parseDocument: %v", err)
	}

	err = validateReferences(&cfg, doc)

	if err == nil {
		t.Fatal("validateReferences accepted three dangling references")
	}
	problems := strings.Split(err.Error(), "\n")
	if len(problems) != 3 {
		t.Fatalf("validateReferences reported %d problems, want 3: %v", len(problems), err)
	}
	for _, want := range []string{
		"model_routes.fast: config_invalid: references unknown model definition",
		"bindings[0].account: binding_invalid: \"undefined-account\" is not a configured channels.discord.accounts name",
		"owner.name: owner_secret_invalid: must not repeat a configured secret value",
	} {
		if !slices.Contains(problems, want) {
			t.Errorf("validateReferences = %v, want the problem %q", problems, want)
		}
	}
	if strings.Contains(err.Error(), canary) {
		t.Errorf("validateReferences leaks the secret value: %v", err)
	}
}

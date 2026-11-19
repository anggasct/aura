package config

import (
	"errors"
	"slices"
	"strings"
	"testing"

	legacyconfig "github.com/anggasct/aura/internal/config"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
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
	cfg.Models.Definitions = map[string]model.Definition{"defined": {}}
	cfg.ModelRoutes = map[string]model.ModelRoute{"fast": {Candidates: []string{"undefined-definition"}}}
	cfg.Bindings = []auraagent.Binding{{Persona: "default", Channel: auraagent.BindingChannelDiscord, Account: "undefined-account", GuildID: "123"}}
	cfg.Owner = &principal.Owner{Name: "owner " + canary}
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

func TestReferenceHelpersGuardNilPointers(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		run  func() error
	}{
		{"webhook nil", "webhook", func() error { return webhookListenProblem(nil, "") }},
		{"telegram discord tokens nil telegram", "channels.telegram", func() error { return telegramDiscordTokenProblems(nil, nil)[0] }},
		{"telegram discord tokens nil discord", "channels.discord", func() error { return telegramDiscordTokenProblems(&legacyconfig.Telegram{}, nil)[0] }},
		{"telegram owner nil telegram", "channels.telegram", func() error { return telegramOwnerProblem(nil, nil) }},
		{"email owner nil email", "channels.email", func() error { return emailOwnerProblem(nil, nil) }},
		{"bindings nil channels", "channels", func() error { return bindingAccountProblems(nil, nil)[0] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("helper accepted a nil pointer, want a coded problem at %q", tc.path)
			}
			if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
				t.Fatalf("helper error = %v, want code %q", err, configkit.ErrorCodeInvalid)
			}
			field, _ := errors.AsType[*configkit.FieldError](err)
			if field == nil || field.Path != tc.path {
				t.Fatalf("helper error = %v, want field path %q", err, tc.path)
			}
		})
	}
}

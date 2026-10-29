package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const soulSecretFixture = "tr0ub4dor-and-3-persona-secret"

func writePersonaConfig(t *testing.T, soulName, soul, definition string) string {
	t.Helper()
	dir := t.TempDir()
	if soulName != "" {
		if err := os.MkdirAll(filepath.Join(dir, "personas"), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "personas", soulName), []byte(soul), 0o600); err != nil {
			t.Fatalf("write soul: %v", err)
		}
	}
	path := filepath.Join(dir, "config.yaml")
	content := "version: 1\nwebhook:\n  keys:\n    - id: hook-1\n      secret_env: TEST_PERSONA_SECRET\nagents:\n  definitions:\n" + definition
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoad_SoulFileResolvesAgainstConfigDirectory(t *testing.T) {
	t.Setenv("TEST_PERSONA_SECRET", soulSecretFixture)
	path := writePersonaConfig(t, "kuma.md", "\n  You are Kuma, casual and brief.\n\n", `    - id: kuma
      description: Casual assistant
      soul_file: personas/kuma.md
`)
	result, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	definition := result.Config.Agents.Definitions[0]
	if definition.Soul != "You are Kuma, casual and brief." || definition.SoulFile != "personas/kuma.md" {
		t.Fatalf("soul = %q from %q, want trimmed text from the relative path", definition.Soul, definition.SoulFile)
	}

	absolute := filepath.Join(filepath.Dir(path), "personas", "kuma.md")
	path = writePersonaConfig(t, "", "", `    - id: kuma
      description: Casual assistant
      soul_file: `+absolute+`
`)
	result, err = Load(path)
	if err != nil || result.Config.Agents.Definitions[0].Soul != "You are Kuma, casual and brief." {
		t.Fatalf("absolute soul_file: %v", err)
	}
}

func TestLoad_SoulFileRejectsInvalidSouls(t *testing.T) {
	t.Setenv("TEST_PERSONA_SECRET", soulSecretFixture)
	cases := map[string]struct {
		soul       string
		definition string
		detail     string
	}{
		"with instructions": {soul: "soul", detail: "mutually exclusive", definition: `    - id: kuma
      description: d
      instructions: inline
      soul_file: personas/kuma.md
`},
		"too large":    {soul: strings.Repeat("a", MaxSoulBytes+1), detail: "exceeds"},
		"not utf8":     {soul: "caf\xc3\x28", detail: "UTF-8"},
		"blank":        {soul: " \n\t ", detail: "empty"},
		"missing":      {detail: "not readable", definition: "    - id: kuma\n      description: d\n      soul_file: personas/absent.md\n"},
		"at the limit": {soul: strings.Repeat("a", MaxSoulBytes)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			definition := tc.definition
			if definition == "" {
				definition = "    - id: kuma\n      description: d\n      soul_file: personas/kuma.md\n"
			}
			_, err := Load(writePersonaConfig(t, "kuma.md", tc.soul, definition))
			if tc.detail == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if code, ok := CodeOf(err); !ok || code != ErrorCodePersonaInvalid {
				t.Fatalf("code = %q, %v (err %v), want persona_invalid", code, ok, err)
			}
			if !strings.Contains(err.Error(), tc.detail) || !strings.Contains(err.Error(), "agents.definitions[0]") {
				t.Fatalf("error %q does not name the field and %q", err, tc.detail)
			}
		})
	}
}

func TestLoad_SoulFileRejectsConfiguredSecret(t *testing.T) {
	t.Setenv("TEST_PERSONA_SECRET", soulSecretFixture)
	path := writePersonaConfig(t, "leaky.md", "You are helpful. Token: "+soulSecretFixture+"\n", `    - id: leaky
      description: d
      soul_file: personas/leaky.md
`)
	_, err := Load(path)
	if code, ok := CodeOf(err); !ok || code != ErrorCodePersonaSecret {
		t.Fatalf("code = %q, %v (err %v), want persona_secret_invalid", code, ok, err)
	}
	if !strings.Contains(err.Error(), `persona "leaky"`) {
		t.Errorf("error does not name the persona: %v", err)
	}
	if strings.Contains(err.Error(), soulSecretFixture) || strings.Contains(err.Error(), "leaky.md") || strings.Contains(err.Error(), "Token") {
		t.Errorf("error leaks soul material: %v", err)
	}
}

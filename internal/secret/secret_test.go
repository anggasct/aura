package secret

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReferenceResolvesFromEnv(t *testing.T) {
	t.Setenv("AURA_TEST_SECRET", "canary-env-1a2b3c")
	value, err := Reference{Env: "AURA_TEST_SECRET"}.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if value != "canary-env-1a2b3c" {
		t.Fatalf("value = %q", value)
	}
}

func TestReferenceResolvesFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api-key")
	if err := os.WriteFile(path, []byte("canary-file-4d5e6f\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	value, err := Reference{File: path}.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if value != "canary-file-4d5e6f" {
		t.Fatalf("value = %q", value)
	}
}

func TestReferenceRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		ref  Reference
	}{
		{name: "no source", ref: Reference{}},
		{name: "both sources", ref: Reference{Env: "A", File: "b"}},
		{name: "missing env", ref: Reference{Env: "AURA_DOES_NOT_EXIST_9f8e7d"}},
		{name: "missing file", ref: Reference{File: filepath.Join(t.TempDir(), "nope")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.ref.Resolve()
			if code, ok := CodeOf(err); !ok || code != ErrorCodeScopeViolation {
				t.Fatalf("Resolve() = %v, want secret_scope_violation", err)
			}
		})
	}
}

// AC-06: canaries must never survive into prompts, logs, traces, errors,
// artifacts, or child environments.
func TestCanariesNeverAppearInChannels(t *testing.T) {
	const (
		apiKey = "sk-canary-a1b2c3d4e5"
		token  = "gh-canary-f6g7h8i9"
	)
	vault := New()
	vault.Set("api_key", apiKey)
	vault.Set("token", token)

	channels := map[string]string{
		"prompt":     "Use the key sk-canary-a1b2c3d4e5 to call the API",
		"log":        "authenticated with token gh-canary-f6g7h8i9",
		"trace":      "span(attrs={api_key: sk-canary-a1b2c3d4e5})",
		"error":      "request failed: invalid secret sk-canary-a1b2c3d4e5",
		"artifact":   `{"headers":{"Authorization":"Bearer gh-canary-f6g7h8i9"}}`,
		"child_env":  "API_KEY=sk-canary-a1b2c3d4e5 TOKEN=gh-canary-f6g7h8i9",
		"arguments":  "tool args: file=/tmp/x token=gh-canary-f6g7h8i9",
		"digest_msg": "sk-canary-a1b2c3d4e5 checksum mismatch",
	}
	for channel, raw := range channels {
		t.Run(channel, func(t *testing.T) {
			redacted := vault.Redact(raw)
			if vault.Contains(redacted) {
				t.Fatalf("canary survived redaction in %s: %q", channel, redacted)
			}
			if redacted == raw {
				t.Fatalf("redaction changed nothing in %s: %q", channel, raw)
			}
		})
	}
}

func TestVaultRawOnlyForAdapter(t *testing.T) {
	vault := New()
	vault.Set("api_key", "canary-raw-0a0b")
	if got := vault.Raw("api_key"); got != "canary-raw-0a0b" {
		t.Fatalf("Raw = %q", got)
	}
	if got := vault.Raw("missing"); got != "" {
		t.Fatalf("Raw(missing) = %q, want empty", got)
	}
	if got := vault.Redact("canary-raw-0a0b"); got == "canary-raw-0a0b" {
		t.Fatalf("Redact did not replace the secret: %q", got)
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeOwnerConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad_OwnerSection(t *testing.T) {
	path := writeOwnerConfig(t, `version: 1
owner:
  name: Angga
  language: Indonesian
  timezone: Asia/Jakarta
  preferences:
    style: concise
    reminder: "senang: pagi"
`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	owner := res.Config.Owner
	if owner == nil {
		t.Fatal("owner section dropped")
	}
	if owner.Name != "Angga" || owner.Language != "Indonesian" || owner.Timezone != "Asia/Jakarta" {
		t.Errorf("owner = %+v", owner)
	}
	if owner.Preferences["style"] != "concise" || owner.Preferences["reminder"] != "senang: pagi" {
		t.Errorf("preferences = %v", owner.Preferences)
	}
}

func TestLoad_OwnerAbsent(t *testing.T) {
	path := writeOwnerConfig(t, "version: 1\n")
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Owner != nil {
		t.Fatalf("owner = %+v, want nil when absent", res.Config.Owner)
	}
}

func TestLoad_OwnerEmptySection(t *testing.T) {
	path := writeOwnerConfig(t, "version: 1\nowner: {}\n")
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Owner == nil {
		t.Fatal("empty owner section dropped")
	}
}

func TestLoad_OwnerEnvOverride(t *testing.T) {
	t.Setenv("AURA_OWNER_NAME", "Angga")
	t.Setenv("AURA_OWNER_TIMEZONE", "Asia/Makassar")
	path := writeOwnerConfig(t, "version: 1\n")
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Owner == nil || res.Config.Owner.Name != "Angga" || res.Config.Owner.Timezone != "Asia/Makassar" {
		t.Fatalf("owner = %+v", res.Config.Owner)
	}
}

func TestLoad_OwnerRejectsBadShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"owner not a mapping", "version: 1\nowner: nope\n"},
		{"name not a string", "version: 1\nowner:\n  name: 7\n"},
		{"preferences not a mapping", "version: 1\nowner:\n  preferences: nope\n"},
		{"preference value not a string", "version: 1\nowner:\n  preferences:\n    style: 7\n"},
		{"unknown key", "version: 1\nowner:\n  nickname: Angga\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeOwnerConfig(t, tc.body)
			if _, err := Load(path); err == nil {
				t.Errorf("invalid owner accepted:\n%s", tc.body)
			}
		})
	}
}

func TestLoad_OwnerRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"invalid timezone", "  timezone: Mars/Olympus\n"},
		{"newline in name", "  name: \"line one\\nline two\"\n"},
		{"untrimmed language", "  language: \" id \"\n"},
		{"empty preference key", "  preferences:\n    \"\": concise\n"},
		{"colon in preference key", "  preferences:\n    \"style:home\": concise\n"},
		{"preference key collides with fixed field", "  preferences:\n    name: Angga\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeOwnerConfig(t, "version: 1\nowner:\n"+tc.body)
			if _, err := Load(path); err == nil {
				t.Errorf("invalid owner accepted:\n%s", tc.body)
			}
		})
	}
}

const ownerSecretFixture = "correct-horse-battery-staple-42"

func TestLoad_OwnerRejectsSecretInFixedField(t *testing.T) {
	t.Setenv("TEST_OWNER_SECRET", ownerSecretFixture)
	path := writeOwnerConfig(t, `version: 1
webhook:
  keys:
    - id: hook-1
      secret_env: TEST_OWNER_SECRET
owner:
  name: "Angga `+ownerSecretFixture+`"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("secret-bearing owner name accepted")
	}
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeOwnerSecret {
		t.Fatalf("code = %v, %v, want owner_secret_invalid", code, ok)
	}
	if !strings.Contains(err.Error(), "owner.name") {
		t.Errorf("error does not name the key: %v", err)
	}
	if strings.Contains(err.Error(), ownerSecretFixture) {
		t.Errorf("error leaks the secret value: %v", err)
	}
}

func TestLoad_OwnerRejectsSecretInPreferences(t *testing.T) {
	t.Setenv("TEST_OWNER_SECRET", ownerSecretFixture)
	path := writeOwnerConfig(t, `version: 1
channels:
  discord:
    bot_token_ref: env://TEST_OWNER_SECRET
owner:
  preferences:
    reminder: "send `+ownerSecretFixture+`"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("secret-bearing preference accepted")
	}
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeOwnerSecret {
		t.Fatalf("code = %v, %v, want owner_secret_invalid", code, ok)
	}
	if !strings.Contains(err.Error(), "owner.preferences") {
		t.Errorf("error does not name the key: %v", err)
	}
	if strings.Contains(err.Error(), ownerSecretFixture) || strings.Contains(err.Error(), "reminder") {
		t.Errorf("error leaks secret material: %v", err)
	}
}

func TestLoad_OwnerRejectsSecretPreferenceKey(t *testing.T) {
	t.Setenv("TEST_OWNER_SECRET", ownerSecretFixture)
	path := writeOwnerConfig(t, `version: 1
channels:
  discord:
    bot_token_ref: env://TEST_OWNER_SECRET
owner:
  preferences:
    "`+ownerSecretFixture+`": concise
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("secret-bearing preference key accepted")
	}
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeOwnerSecret {
		t.Fatalf("code = %v, %v, want owner_secret_invalid", code, ok)
	}
	if strings.Contains(err.Error(), ownerSecretFixture) {
		t.Errorf("error leaks the secret value: %v", err)
	}
}

func TestLoad_OwnerPassesWhenValuesDoNotMatchSecrets(t *testing.T) {
	t.Setenv("TEST_OWNER_SECRET", ownerSecretFixture)
	path := writeOwnerConfig(t, `version: 1
channels:
  discord:
    bot_token_ref: env://TEST_OWNER_SECRET
owner:
  name: Angga
  preferences:
    reminder: "senang: pagi"
`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Owner == nil || res.Config.Owner.Name != "Angga" {
		t.Fatalf("owner = %+v", res.Config.Owner)
	}
}

func TestLoad_OwnerPassesWhenReferencedSecretUnset(t *testing.T) {
	path := writeOwnerConfig(t, `version: 1
channels:
  discord:
    bot_token_ref: env://TEST_OWNER_SECRET_UNSET
owner:
  name: Angga
  preferences:
    reminder: "senang: pagi"
`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Owner == nil || res.Config.Owner.Name != "Angga" {
		t.Fatalf("owner = %+v", res.Config.Owner)
	}
}

func TestLoad_OwnerIdentities(t *testing.T) {
	path := writeOwnerConfig(t, "version: 1\nowner:\n  name: Angga\n  identities:\n    - discord:123456789012345678\n")
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Owner.Identities; len(got) != 1 || got[0] != "discord:123456789012345678" {
		t.Errorf("identities = %v", got)
	}
}

func TestLoad_OwnerIdentitiesRejected(t *testing.T) {
	cases := map[string]struct {
		body  string
		index string
		value string
	}{
		"unknown channel prefix": {body: "    - telegram:5550001\n", index: "owner.identities[0]", value: "5550001"},
		"empty discord id":       {body: "    - \"discord:\"\n", index: "owner.identities[0]"},
		"non-numeric discord id": {body: "    - discord:5550001\n    - discord:owner5550002\n", index: "owner.identities[1]", value: "owner5550002"},
		"duplicate identity":     {body: "    - discord:5550003\n    - discord:5550003\n", index: "owner.identities[1]", value: "5550003"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeOwnerConfig(t, "version: 1\nowner:\n  identities:\n"+tc.body)
			_, err := Load(path)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeOwnerIdentityInvalid {
				t.Fatalf("code = %q, %v; want %q (err %v)", code, ok, ErrorCodeOwnerIdentityInvalid, err)
			}
			if !strings.Contains(err.Error(), tc.index) {
				t.Errorf("error %q does not name %s", err, tc.index)
			}
			if tc.value != "" && strings.Contains(err.Error(), tc.value) {
				t.Errorf("error %q leaks the identity value", err)
			}
		})
	}
}

func TestLoad_OwnerIdentitiesRenderSingleCode(t *testing.T) {
	path := writeOwnerConfig(t, "version: 1\nowner:\n  identities:\n    - telegram:5550001\n    - discord:owner5550002\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("invalid identities accepted")
	}
	if code, ok := CodeOf(err); !ok || code != ErrorCodeOwnerIdentityInvalid {
		t.Fatalf("code = %q, %v; want %q (err %v)", code, ok, ErrorCodeOwnerIdentityInvalid, err)
	}
	if got := strings.Count(err.Error(), string(ErrorCodeOwnerIdentityInvalid)+":"); got != 1 {
		t.Errorf("error %q carries %d code prefixes; want 1", err, got)
	}
	if !strings.Contains(err.Error(), string(ErrorCodeOwnerIdentityInvalid)+": owner.identities[0]:") {
		t.Errorf("error %q does not render the code once before the first index", err)
	}
	for _, index := range []string{"owner.identities[0]", "owner.identities[1]"} {
		if !strings.Contains(err.Error(), index) {
			t.Errorf("error %q does not name %s", err, index)
		}
	}
	for _, value := range []string{"5550001", "owner5550002"} {
		if strings.Contains(err.Error(), value) {
			t.Errorf("error %q leaks the identity value %s", err, value)
		}
	}
}

func TestLoad_OwnerIdentitiesRejectBadShapes(t *testing.T) {
	for name, body := range map[string]string{
		"not a list":         "  identities: discord:5550001\n",
		"entry not a string": "  identities:\n    - 5550001\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeOwnerConfig(t, "version: 1\nowner:\n"+body)); err == nil {
				t.Errorf("invalid identities accepted:\n%s", body)
			}
		})
	}
}

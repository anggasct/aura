package config

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoad_PersonaAccessValidation(t *testing.T) {
	t.Setenv("TEST_PERSONA_SECRET", soulSecretFixture)
	cases := map[string]struct {
		fields string
		code   ErrorCode
		detail string
	}{
		"defaults":                         {},
		"trusted with pairing":             {fields: "      audience: trusted\n      pairing: true\n"},
		"public":                           {fields: "      audience: public\n"},
		"unknown audience":                 {fields: "      audience: everyone\n", code: ErrorCodePersonaInvalid, detail: "audience"},
		"pairing on public":                {fields: "      audience: public\n      pairing: true\n", code: ErrorCodePersonaInvalid, detail: "pairing"},
		"pairing on owner":                 {fields: "      pairing: true\n", code: ErrorCodePersonaInvalid, detail: "pairing"},
		"duplicate principal tool":         {fields: "      principal_tools: [web_fetch, web_fetch]\n", code: ErrorCodePersonaInvalid, detail: "principal_tools"},
		"approval timeout in range":        {fields: "      approval_timeout: 10m\n"},
		"approval timeout too short":       {fields: "      approval_timeout: 30s\n", code: ErrorCodeConfigInvalid, detail: "approval_timeout"},
		"approval timeout too long":        {fields: "      approval_timeout: 25h\n", code: ErrorCodeConfigInvalid, detail: "approval_timeout"},
		"unknown message key":              {fields: "      messages:\n        hello: hi\n", code: ErrorCodePersonaInvalid, detail: `unknown key "hello"`},
		"empty message":                    {fields: "      messages:\n        waiting_for_owner: \" \"\n", code: ErrorCodePersonaInvalid, detail: "must not be empty"},
		"multi line message":               {fields: "      messages:\n        waiting_for_owner: \"a\\nb\"\n", code: ErrorCodePersonaInvalid, detail: "single line"},
		"overlong message":                 {fields: "      messages:\n        waiting_for_owner: " + strings.Repeat("x", MaxPersonaMessageRunes+1) + "\n", code: ErrorCodePersonaInvalid, detail: "at most"},
		"pairing code without placeholder": {fields: "      messages:\n        pairing_code: here is your code\n", code: ErrorCodePersonaInvalid, detail: "{code}"},
		"message at the length limit":      {fields: "      messages:\n        waiting_for_owner: " + strings.Repeat("x", MaxPersonaMessageRunes) + "\n"},
		"shared group on trusted":          {fields: "      audience: trusted\n      group_context: shared\n"},
		"per principal group on public":    {fields: "      audience: public\n      group_context: per_principal\n"},
		"shared group on public":           {fields: "      audience: public\n      group_context: shared\n", code: ErrorCodePersonaInvalid, detail: "group_context"},
		"shared group on owner":            {fields: "      group_context: shared\n", code: ErrorCodePersonaInvalid, detail: "group_context"},
		"unknown group context":            {fields: "      audience: trusted\n      group_context: everyone\n", code: ErrorCodePersonaInvalid, detail: "group_context"},
	}
	for label, tc := range cases {
		t.Run(label, func(t *testing.T) {
			definition := "    - id: kantor\n      description: d\n      instructions: i\n" + tc.fields
			_, err := Load(writePersonaConfig(t, "", "", definition))
			if tc.code == "" {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if code, ok := CodeOf(err); !ok || code != tc.code {
				t.Fatalf("code = %q, %v (err %v), want %s", code, ok, err, tc.code)
			}
			if !strings.Contains(err.Error(), "agents.definitions[0]") || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("error %q does not name the field and %q", err, tc.detail)
			}
		})
	}
}

func TestLoad_PersonaMessagesOverrideEveryKey(t *testing.T) {
	t.Setenv("TEST_PERSONA_SECRET", soulSecretFixture)
	defaults := PersonaMessageDefaults()
	keys := slices.Sorted(maps.Keys(defaults))
	if len(keys) != 13 {
		t.Fatalf("message keys = %v, want the 13 fixed replies", keys)
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			value := "custom " + key
			if key == MessagePairingCode {
				value += " " + PairingCodePlaceholder
			}
			definition := "    - id: kantor\n      description: d\n      instructions: i\n      messages:\n        " + key + ": " + value + "\n"
			res, err := Load(writePersonaConfig(t, "", "", definition))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := res.Config.Agents.Definitions[0].Messages[key]; got != value {
				t.Fatalf("messages.%s = %q", key, got)
			}
			if strings.TrimSpace(defaults[key]) == "" {
				t.Fatalf("default for %s is empty", key)
			}
		})
	}
}

func writeAccessConfig(t *testing.T, access string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 1\n"
	if access != "" {
		content += "access:\n" + access
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoad_AccessSection(t *testing.T) {
	res, err := Load(writeAccessConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if res.Config.Access == nil || time.Duration(res.Config.Access.ApprovalTimeout) != 30*time.Minute || time.Duration(res.Config.Access.StandingGrantTTL) != 168*time.Hour {
		t.Fatalf("access defaults = %+v, want 30m and 168h", res.Config.Access)
	}
	res, err = Load(writeAccessConfig(t, "  approval_timeout: 10m\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if time.Duration(res.Config.Access.ApprovalTimeout) != 10*time.Minute || time.Duration(res.Config.Access.StandingGrantTTL) != 168*time.Hour {
		t.Fatalf("partial access = %+v, want 10m and default ttl", res.Config.Access)
	}
	for label, access := range map[string]string{
		"approval timeout too short":  "  approval_timeout: 59s\n",
		"approval timeout too long":   "  approval_timeout: 24h1s\n",
		"standing grant ttl too long": "  standing_grant_ttl: 721h\n",
		"standing grant ttl zero":     "  standing_grant_ttl: 0s\n",
	} {
		t.Run(label, func(t *testing.T) {
			_, err := Load(writeAccessConfig(t, access))
			if code, ok := CodeOf(err); !ok || code != ErrorCodeConfigInvalid {
				t.Fatalf("code = %q, %v (err %v), want config_invalid", code, ok, err)
			}
		})
	}
	if _, err := Load(writeAccessConfig(t, "  standing_grant_ttl: 720h\n")); err != nil {
		t.Fatalf("Load at the ttl limit: %v", err)
	}
	if _, err := Load(writeAccessConfig(t, "  unknown: 1\n")); err == nil || !strings.Contains(err.Error(), "access.unknown") {
		t.Fatalf("unknown access key err = %v", err)
	}
	t.Setenv("AURA_ACCESS_APPROVAL_TIMEOUT", "5m")
	res, err = Load(writeAccessConfig(t, ""))
	if err != nil {
		t.Fatalf("Load with env override: %v", err)
	}
	if time.Duration(res.Config.Access.ApprovalTimeout) != 5*time.Minute {
		t.Fatalf("env approval_timeout = %v, want 5m", time.Duration(res.Config.Access.ApprovalTimeout))
	}
}

func TestLoad_AccessBudgets(t *testing.T) {
	res, err := Load(writeAccessConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := AccessBudgets{
		Trusted: PrincipalBudget{MaxTokensPerDay: 200000, MaxTurnsPerMinute: 6},
		Public:  PrincipalBudget{MaxTokensPerDay: 20000, MaxTurnsPerMinute: 3},
	}
	if res.Config.Access.Budgets != want {
		t.Fatalf("budget defaults = %+v, want %+v", res.Config.Access.Budgets, want)
	}
	res, err = Load(writeAccessConfig(t, "  budgets:\n    public:\n      max_tokens_per_day: 5000\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Access.Budgets; got.Public != (PrincipalBudget{MaxTokensPerDay: 5000, MaxTurnsPerMinute: 3}) || got.Trusted != want.Trusted {
		t.Fatalf("partial budgets = %+v, want public tokens 5000 and other defaults", got)
	}
	for label, access := range map[string]string{
		"zero tokens":         "  budgets:\n    trusted:\n      max_tokens_per_day: 0\n",
		"negative turns":      "  budgets:\n    public:\n      max_turns_per_minute: -1\n",
		"string tokens":       "  budgets:\n    public:\n      max_tokens_per_day: lots\n",
		"budgets not mapping": "  budgets: 5\n",
		"class not mapping":   "  budgets:\n    public: 5\n",
	} {
		t.Run(label, func(t *testing.T) {
			_, err := Load(writeAccessConfig(t, access))
			if err == nil || !strings.Contains(err.Error(), "access.budgets") {
				t.Fatalf("err = %v, want an access.budgets error", err)
			}
		})
	}
	for label, access := range map[string]string{
		"unknown class": "  budgets:\n    owner:\n      max_tokens_per_day: 1\n",
		"unknown limit": "  budgets:\n    public:\n      max_cost: 1\n",
	} {
		t.Run(label, func(t *testing.T) {
			if _, err := Load(writeAccessConfig(t, access)); err == nil || !strings.Contains(err.Error(), "access.budgets") {
				t.Fatalf("err = %v, want the unknown key rejected", err)
			}
		})
	}
}

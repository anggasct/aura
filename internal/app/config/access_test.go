package config

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
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
		"unknown audience":                 {fields: "      audience: everyone\n", code: personaInvalidCode, detail: "audience"},
		"pairing on public":                {fields: "      audience: public\n      pairing: true\n", code: personaInvalidCode, detail: "pairing"},
		"pairing on owner":                 {fields: "      pairing: true\n", code: personaInvalidCode, detail: "pairing"},
		"duplicate principal tool":         {fields: "      principal_tools: [web_fetch, web_fetch]\n", code: personaInvalidCode, detail: "principal_tools"},
		"approval timeout in range":        {fields: "      approval_timeout: 10m\n"},
		"approval timeout too short":       {fields: "      approval_timeout: 30s\n", code: invalidCode, detail: "approval_timeout"},
		"approval timeout too long":        {fields: "      approval_timeout: 25h\n", code: invalidCode, detail: "approval_timeout"},
		"unknown message key":              {fields: "      messages:\n        hello: hi\n", code: personaInvalidCode, detail: `unknown key "hello"`},
		"empty message":                    {fields: "      messages:\n        waiting_for_owner: \" \"\n", code: personaInvalidCode, detail: "must not be empty"},
		"multi line message":               {fields: "      messages:\n        waiting_for_owner: \"a\\nb\"\n", code: personaInvalidCode, detail: "single line"},
		"overlong message":                 {fields: "      messages:\n        waiting_for_owner: " + strings.Repeat("x", auraagent.MaxPersonaMessageRunes+1) + "\n", code: personaInvalidCode, detail: "at most"},
		"pairing code without placeholder": {fields: "      messages:\n        pairing_code: here is your code\n", code: personaInvalidCode, detail: "{code}"},
		"message at the length limit":      {fields: "      messages:\n        waiting_for_owner: " + strings.Repeat("x", auraagent.MaxPersonaMessageRunes) + "\n"},
		"shared group on trusted":          {fields: "      audience: trusted\n      group_context: shared\n"},
		"per principal group on public":    {fields: "      audience: public\n      group_context: per_principal\n"},
		"shared group on public":           {fields: "      audience: public\n      group_context: shared\n", code: personaInvalidCode, detail: "group_context"},
		"shared group on owner":            {fields: "      group_context: shared\n", code: personaInvalidCode, detail: "group_context"},
		"unknown group context":            {fields: "      audience: trusted\n      group_context: everyone\n", code: personaInvalidCode, detail: "group_context"},
		"evals dir":                        {fields: "      evals:\n        dir: evals/kantor\n"},
		"workflows allowlist":              {fields: "      workflows: [book_visit, refund]\n"},
		"plugins allowlist":                {fields: "      plugins: [weather, mail-reader]\n"},
		"plugins duplicate":                {fields: "      plugins: [weather, weather]\n", code: personaInvalidCode, detail: "twice"},
		"plugins invalid id":               {fields: "      plugins: [Weather]\n", code: personaInvalidCode, detail: "plugin ids"},
		"plugins path like id":             {fields: "      plugins: [\"../x\"]\n", code: personaInvalidCode, detail: "plugin ids"},
		"workflows duplicate":              {fields: "      workflows: [book_visit, book_visit]\n", code: personaInvalidCode, detail: "twice"},
		"workflows empty entry":            {fields: "      workflows: [\" \"]\n", code: personaInvalidCode, detail: "workflows"},
		"evals behavior":                   {fields: "      evals:\n        behavior:\n          required: true\n          min_score: 0.75\n          judge_route: primary\n          schedule: \"0 3 * * 1\"\n          enforce_on_regression: true\n"},
		"evals behavior min score 1":       {fields: "      evals:\n        behavior:\n          min_score: 1\n"},
		"evals behavior min score zero":    {fields: "      evals:\n        behavior:\n          min_score: 0\n", code: invalidCode, detail: "min_score"},
		"evals behavior min score above 1": {fields: "      evals:\n        behavior:\n          min_score: 1.5\n", code: invalidCode, detail: "min_score"},
		"evals behavior min score text":    {fields: "      evals:\n        behavior:\n          min_score: high\n", code: invalidCode, detail: "min_score"},
		"evals behavior required text":     {fields: "      evals:\n        behavior:\n          required: yes please\n", code: invalidCode, detail: "required"},
		"evals behavior empty judge":       {fields: "      evals:\n        behavior:\n          judge_route: \"\"\n", code: invalidCode, detail: "judge_route"},
		"evals behavior unknown key":       {fields: "      evals:\n        behavior:\n          judge: primary\n", code: unknownKeyCode, detail: "evals.behavior.judge"},
		"evals behavior not a mapping":     {fields: "      evals:\n        behavior: true\n", code: invalidCode, detail: "evals.behavior"},
		"evals unknown key":                {fields: "      evals:\n        directory: x\n", code: unknownKeyCode, detail: "evals.directory"},
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
			if code, ok := codeOf(err); !ok || code != tc.code {
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
	defaults := auraagent.PersonaMessageDefaults()
	keys := slices.Sorted(maps.Keys(defaults))
	if len(keys) != 65 {
		t.Fatalf("message keys = %d (%v), want the 65 fixed replies", len(keys), keys)
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			parts := append([]string{"custom " + key}, auraagent.MessagePlaceholders(key)...)
			value := strings.Join(parts, " ")
			definition := "    - id: kantor\n      description: d\n      instructions: i\n      messages:\n        " + key + ": " + strconv.Quote(value) + "\n"
			res, err := Load(writePersonaConfig(t, "", "", definition))
			if auraagent.IsBundleOnlyMessage(key) {
				if code, ok := codeOf(err); !ok || code != personaInvalidCode {
					t.Fatalf("bundle-only key %s: code = %q, %v (err %v), want %s", key, code, ok, err, personaInvalidCode)
				}
				return
			}
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

func TestCardOverridesKeepPlaceholdersAndBundleOnlyKeys(t *testing.T) {
	t.Setenv("TEST_PERSONA_SECRET", soulSecretFixture)
	cases := map[string]struct {
		key    string
		value  string
		wantOK bool
		detail string
	}{
		"approval_summary without persona": {
			key:    auraagent.MessageApprovalSummary,
			value:  "Wants to {action}:",
			detail: "{persona}",
		},
		"approval_summary without action": {
			key:    auraagent.MessageApprovalSummary,
			value:  "{persona} wants to act:",
			detail: "{action}",
		},
		"approval_summary valid": {
			key:    auraagent.MessageApprovalSummary,
			value:  "{persona} asks to {action}:",
			wantOK: true,
		},
		"bundle-only approval_action_generic rejected": {
			key:    auraagent.MessageApprovalActionGeneric,
			value:  "run {tool}",
			detail: "cannot be overridden",
		},
		"approval_requested_by without requester": {
			key:    auraagent.MessageApprovalRequestedBy,
			value:  "Asked by someone",
			detail: "{requester}",
		},
		"approval_workflow_run without run": {
			key:    auraagent.MessageApprovalWorkflowRun,
			value:  "Workflow run",
			detail: "{run}",
		},
		"button_allow_for without duration": {
			key:    auraagent.MessageButtonAllowFor,
			value:  "Allow always",
			detail: "{duration}",
		},
		"duration_days without n": {
			key:    auraagent.MessageDurationDays,
			value:  "some days",
			detail: "{n}",
		},
		"duration_hours without n": {
			key:    auraagent.MessageDurationHours,
			value:  "some hours",
			detail: "{n}",
		},
		"card_approved without time": {
			key:    auraagent.MessageCardApproved,
			value:  "✅ Approved",
			detail: "{time}",
		},
		"card_allowed_for without duration": {
			key:    auraagent.MessageCardAllowedFor,
			value:  "✅ Allowed · {time}",
			detail: "{duration}",
		},
		"card_allowed_for without time": {
			key:    auraagent.MessageCardAllowedFor,
			value:  "✅ Allowed for {duration}",
			detail: "{time}",
		},
		"bundle-only approval_action_exec rejected": {
			key:    auraagent.MessageApprovalActionExec,
			value:  "do something safe",
			detail: "cannot be overridden",
		},
		"bundle-only approval_untrusted_input rejected": {
			key:    auraagent.MessageApprovalUntrustedInput,
			value:  "safe input",
			detail: "cannot be overridden",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			definition := "    - id: kantor\n      description: d\n      instructions: i\n      messages:\n        " + tc.key + ": " + strconv.Quote(tc.value) + "\n"
			_, err := Load(writePersonaConfig(t, "", "", definition))
			if tc.wantOK {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if code, ok := codeOf(err); !ok || code != personaInvalidCode {
				t.Fatalf("code = %q, %v (err %v), want %s", code, ok, err, personaInvalidCode)
			}
			if !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("err = %q, want %q", err, tc.detail)
			}
		})
	}
}

func TestPersonaOverrideMustKeepItsPlaceholders(t *testing.T) {
	t.Setenv("TEST_PERSONA_SECRET", soulSecretFixture)
	for name, tc := range map[string]struct {
		key    string
		value  string
		wantOK bool
		detail string
	}{
		"workflow_succeeded without goal": {
			key:    auraagent.MessageWorkflowSucceeded,
			value:  "All done!",
			detail: "{goal}",
		},
		"workflow_failed without goal": {
			key:    auraagent.MessageWorkflowFailed,
			value:  "It failed.",
			detail: "{goal}",
		},
		"workflow_cancelled without goal": {
			key:    auraagent.MessageWorkflowCancelled,
			value:  "Cancelled.",
			detail: "{goal}",
		},
		"pairing_code without code": {
			key:    auraagent.MessagePairingCode,
			value:  "Ask the owner.",
			detail: "{code}",
		},
		"unexpected placeholder on plain key": {
			key:    auraagent.MessageUnavailable,
			value:  "Unavailable {code}",
			detail: "{code}",
		},
		"workflow_succeeded with goal": {
			key:    auraagent.MessageWorkflowSucceeded,
			value:  "Selesai: {goal}",
			wantOK: true,
		},
		"status_working accepted": {
			key:    auraagent.MessageStatusWorking,
			value:  "Lagi dikerjakan…",
			wantOK: true,
		},
		"status_still_working accepted": {
			key:    auraagent.MessageStatusStillWorking,
			value:  "Masih lanjut…",
			wantOK: true,
		},
		"status_stopped accepted": {
			key:    auraagent.MessageStatusStopped,
			value:  "⏹ Berhenti",
			wantOK: true,
		},
		"button_stop accepted": {
			key:    auraagent.MessageButtonStop,
			value:  "Berhenti",
			wantOK: true,
		},
		"command_status_persona without persona": {
			key:    auraagent.MessageCommandStatusPersona,
			value:  "Persona:",
			detail: "{persona}",
		},
		"command_status_running_queued without count": {
			key:    auraagent.MessageCommandStatusRunningQueued,
			value:  "Working on a request; more waiting.",
			detail: "{count}",
		},
		"command_unavailable without command": {
			key:    auraagent.MessageCommandUnavailable,
			value:  "Only works in the terminal.",
			detail: "{command}",
		},
		"skill_unavailable without skill": {
			key:    auraagent.MessageSkillUnavailable,
			value:  "Skill is not available.",
			detail: "{skill}",
		},
		"command_usage without usage": {
			key:    auraagent.MessageCommandUsage,
			value:  "Usage: /{command}",
			detail: "{usage}",
		},
		"command_usage with command and usage": {
			key:    auraagent.MessageCommandUsage,
			value:  "Cara pakai: /{command} {usage}",
			wantOK: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			definition := "    - id: kantor\n      description: d\n      instructions: i\n      messages:\n        " + tc.key + ": " + strconv.Quote(tc.value) + "\n"
			path := writePersonaConfig(t, "", "", definition)
			_, err := Load(path)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				return
			}
			if code, ok := codeOf(err); !ok || code != personaInvalidCode {
				t.Fatalf("code = %q, %v (err %v), want %s", code, ok, err, personaInvalidCode)
			}
			if !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("err = %q, want %q", err, tc.detail)
			}
		})
	}

	resolved := resolvedSchema(t)
	doc := []byte("version: 1\nowner:\n  language: id\nagents:\n  definitions:\n    - id: kantor\n      description: d\n      instructions: i\n      messages:\n        status_working: Lagi jalan\n        status_still_working: Masih jalan\n        status_stopped: Berhenti\n        button_stop: Stop\n")
	if err := resolved.Validate(jsonInstance(t, doc)); err != nil {
		t.Fatalf("schema refused new message keys: %v", err)
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
			if code, ok := codeOf(err); !ok || code != invalidCode {
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
	want := principal.Budgets{
		Trusted: principal.Budget{MaxTokensPerDay: 200000, MaxTurnsPerMinute: 6},
		Public:  principal.Budget{MaxTokensPerDay: 20000, MaxTurnsPerMinute: 3},
	}
	if res.Config.Access.Budgets != want {
		t.Fatalf("budget defaults = %+v, want %+v", res.Config.Access.Budgets, want)
	}
	res, err = Load(writeAccessConfig(t, "  budgets:\n    public:\n      max_tokens_per_day: 5000\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Access.Budgets; got.Public != (principal.Budget{MaxTokensPerDay: 5000, MaxTurnsPerMinute: 3}) || got.Trusted != want.Trusted {
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

func TestLoad_AccessRetention(t *testing.T) {
	res, err := Load(writeAccessConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Access.Retention; time.Duration(got.Trusted) != 4320*time.Hour || time.Duration(got.Public) != 720*time.Hour {
		t.Fatalf("retention defaults = %+v, want 4320h and 720h", got)
	}
	res, err = Load(writeAccessConfig(t, "  retention:\n    public: 48h\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := res.Config.Access.Retention; time.Duration(got.Public) != 48*time.Hour || time.Duration(got.Trusted) != 4320*time.Hour {
		t.Fatalf("partial retention = %+v", got)
	}
	for label, access := range map[string]string{
		"below a day":   "  retention:\n    public: 23h\n",
		"zero":          "  retention:\n    trusted: 0s\n",
		"integer value": "  retention:\n    public: 720\n",
		"not a mapping": "  retention: 720h\n",
		"unknown class": "  retention:\n    owner: 720h\n",
	} {
		t.Run(label, func(t *testing.T) {
			if _, err := Load(writeAccessConfig(t, access)); err == nil || !strings.Contains(err.Error(), "access.retention") {
				t.Fatalf("err = %v, want an access.retention error", err)
			}
		})
	}
}

func TestLoad_AccessHandoffTimeout(t *testing.T) {
	res, err := Load(writeAccessConfig(t, ""))
	if err != nil || time.Duration(res.Config.Access.HandoffTimeout) != 24*time.Hour {
		t.Fatalf("handoff_timeout default = %v, %v; want 24h", res.Config.Access.HandoffTimeout, err)
	}
	if res, err := Load(writeAccessConfig(t, "  handoff_timeout: 2h\n")); err != nil || time.Duration(res.Config.Access.HandoffTimeout) != 2*time.Hour {
		t.Fatalf("handoff_timeout override = %v", err)
	}
	for _, access := range []string{"  handoff_timeout: 30s\n", "  handoff_timeout: 721h\n", "  handoff_timeout: 5\n"} {
		if _, err := Load(writeAccessConfig(t, access)); err == nil || !strings.Contains(err.Error(), "handoff_timeout") {
			t.Errorf("Load(%q) err = %v, want a handoff_timeout error", access, err)
		}
	}
}

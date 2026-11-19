package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestInvalidSecretReferenceFailsAtDecodeWithItsPosition(t *testing.T) {
	const canary = "s05-canary-value"
	cases := []struct {
		name    string
		path    string
		content string
	}{
		{"discord bot token", "channels.discord.accounts[0].bot_token_ref", "version: 1\nchannels:\n  discord:\n    accounts:\n      - name: home\n        bot_token_ref: " + canary + "\n"},
		{"telegram bot token", "channels.telegram.accounts[0].bot_token_ref", "version: 1\nchannels:\n  telegram:\n    accounts:\n      - name: pocket\n        bot_token_ref: " + canary + "\n"},
		{"telegram webhook secret", "channels.telegram.accounts[0].webhook_secret_ref", "version: 1\nchannels:\n  telegram:\n    accounts:\n      - name: pocket\n        webhook_secret_ref: file://" + canary + "\n"},
		{"email imap password", "channels.email.accounts[0].imap.password_ref", "version: 1\nchannels:\n  email:\n    accounts:\n      - name: work\n        imap:\n          password_ref: env://" + canary + "\n"},
		{"email smtp password", "channels.email.accounts[0].smtp.password_ref", "version: 1\nchannels:\n  email:\n    accounts:\n      - name: work\n        smtp:\n          password_ref: https://" + canary + "\n"},
		{"sync transport secret", "sync.transport_secret_ref", "version: 1\nsync:\n  transport_secret_ref: " + canary + "\n"},
		{"sync known hosts", "sync.known_hosts_ref", "version: 1\nsync:\n  known_hosts_ref: file://" + canary + "\n"},
		{"telemetry credential", "telemetry.credential_ref", "version: 1\ntelemetry:\n  credential_ref: " + canary + "\n"},
		{"web search credential", "tools.web_search.credential_ref", "version: 1\ntools:\n  web_search:\n    credential_ref: " + canary + "\n"},
		{"knowledge embedding credential", "knowledge.embedding.credential_ref", "version: 1\nknowledge:\n  embedding:\n    credential_ref: " + canary + "\n"},
		{"github credential", "tools.github.credentials[0].ref", "version: 1\ntools:\n  github:\n    credentials:\n      - name: ci\n        ref: " + canary + "\n"},
		{"model api key", "models.definitions[primary].api_key_ref", "version: 1\nmodels:\n  definitions:\n    primary:\n      api_key_ref: " + canary + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, column := locate(t, tc.content, canary)
			_, err := Load(writeTempConfig(t, tc.content))
			if err == nil {
				t.Fatal("Load accepted an invalid secret reference")
			}
			want := fmt.Sprintf("%s (line %d, column %d): config_invalid: ", tc.path, line, column)
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want it to contain %q", err, want)
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("error echoes the text of the reference: %v", err)
			}
		})
	}
}

func locate(t *testing.T, content, needle string) (line, column int) {
	t.Helper()
	for i, text := range strings.Split(content, "\n") {
		if strings.Contains(text, needle) {
			return i + 1, strings.Index(text, ": ") + len(": ") + 1
		}
	}
	t.Fatalf("fixture does not contain %q", needle)
	return 0, 0
}

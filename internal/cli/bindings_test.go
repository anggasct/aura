package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func writeBindingsCLIConfig(t *testing.T) string {
	t.Helper()
	return writeAgentsConfig(t, `storage:
  path: `+t.TempDir()+`
channels:
  discord:
    accounts:
      - name: home
        bot_token_ref: env://DISCORD_HOME_TOKEN
        persona: main
      - name: kuma
        bot_token_ref: env://DISCORD_KUMA_TOKEN
bindings:
  - persona: engineer
    channel: discord
    account: home
    channel_id: "123"
  - persona: reviewer
    channel: discord
    account: home
    guild_id: "77"
    channel_id: "123"
    user_id: "456"
`)
}

func runBindingsExplain(t *testing.T, configPath string, args ...string) (string, error) {
	t.Helper()
	cmd := newBindingsCmd(&globalFlags{configPath: configPath})
	cmd.SilenceUsage = true
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"explain"}, args...))
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestBindingsExplainPrintsWinningBinding(t *testing.T) {
	path := writeBindingsCLIConfig(t)
	cases := map[string]struct {
		args []string
		want string
	}{
		"most specific binding": {
			args: []string{"--channel", "discord", "--account", "home", "--guild", "77", "--channel-id", "123", "--user", "456"},
			want: "persona: reviewer\nscore: 22\nbinding: bindings[1] guild_id=77 channel_id=123 user_id=456\n",
		},
		"channel binding": {
			args: []string{"--channel", "discord", "--account", "home", "--guild", "77", "--channel-id", "123", "--user", "999"},
			want: "persona: engineer\nscore: 4\nbinding: bindings[0] channel_id=123\n",
		},
		"account default": {
			args: []string{"--account", "home", "--channel-id", "555"},
			want: "persona: main\nscore: 1\nbinding: account default (discord:home)\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := runBindingsExplain(t, path, tc.args...)
			if err != nil {
				t.Fatalf("bindings explain: %v", err)
			}
			if out != tc.want {
				t.Fatalf("output = %q, want %q", out, tc.want)
			}
		})
	}
}

func TestBindingsExplainNoPersonaExitsThree(t *testing.T) {
	out, err := runBindingsExplain(t, writeBindingsCLIConfig(t), "--channel", "discord", "--account", "kuma", "--channel-id", "123")
	var coded *exitCodeError
	if !errors.As(err, &coded) || coded.code != exitNoPersona {
		t.Fatalf("error = %v, want exit code %d", err, exitNoPersona)
	}
	if strings.TrimSpace(out) != "no persona" {
		t.Fatalf("output = %q, want no persona", out)
	}
}

func TestBindingsExplainRejectsBadScope(t *testing.T) {
	path := writeBindingsCLIConfig(t)
	cases := map[string][]string{
		"missing account":     {"--channel", "discord"},
		"unsupported channel": {"--channel", "telegram", "--account", "home"},
		"unknown account":     {"--account", "work"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := runBindingsExplain(t, path, args...)
			var usage *usageError
			if !errors.As(err, &usage) {
				t.Fatalf("error = %v, want usage error", err)
			}
		})
	}
}

func TestBindingsExplainRejectsUnregisteredPersona(t *testing.T) {
	path := writeAgentsConfig(t, `storage:
  path: `+t.TempDir()+`
channels:
  discord:
    accounts:
      - name: home
        bot_token_ref: env://DISCORD_HOME_TOKEN
bindings:
  - persona: ghost
    channel: discord
    account: home
    channel_id: "123"
`)
	_, err := runBindingsExplain(t, path, "--account", "home", "--channel-id", "123")
	if err == nil || !strings.Contains(err.Error(), "binding_invalid") {
		t.Fatalf("error = %v, want binding_invalid", err)
	}
	var usage *usageError
	if errors.As(err, &usage) {
		t.Fatal("a config validation failure must not be a usage error")
	}
}

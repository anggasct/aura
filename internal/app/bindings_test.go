package app

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
  telegram:
    accounts:
      - name: pocket
        bot_token_ref: env://TELEGRAM_POCKET_TOKEN
        persona: main
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
  - persona: engineer
    channel: telegram
    account: pocket
    chat_id: "-1001234567890"
  - persona: reviewer
    channel: telegram
    account: pocket
    chat_id: "-1001234567890"
    thread_id: "42"
  - persona: reviewer
    channel: telegram
    account: pocket
    user_id: "424242"
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
		"telegram chat binding": {
			args: []string{"--channel", "telegram", "--account", "pocket", "--chat", "-1001234567890", "--user", "999001"},
			want: "persona: engineer\nscore: 4\nbinding: bindings[2] chat_id=-1001234567890\n",
		},
		"telegram forum topic binding": {
			args: []string{"--channel", "telegram", "--account", "pocket", "--chat", "-1001234567890", "--topic", "42", "--user", "999001"},
			want: "persona: reviewer\nscore: 12\nbinding: bindings[3] chat_id=-1001234567890 thread_id=42\n",
		},
		"telegram private chat user binding": {
			args: []string{"--channel", "telegram", "--account", "pocket", "--user", "424242"},
			want: "persona: reviewer\nscore: 16\nbinding: bindings[4] user_id=424242\n",
		},
		"telegram account default": {
			args: []string{"--channel", "telegram", "--account", "pocket", "--user", "999001"},
			want: "persona: main\nscore: 1\nbinding: account default (telegram:pocket)\n",
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
		"missing account":          {"--channel", "discord"},
		"unsupported channel":      {"--channel", "slack", "--account", "home"},
		"unknown account":          {"--account", "work"},
		"unknown telegram account": {"--channel", "telegram", "--account", "home"},
		"discord flag on telegram": {"--channel", "telegram", "--account", "pocket", "--guild", "77"},
		"thread flag on telegram":  {"--channel", "telegram", "--account", "pocket", "--thread", "42"},
		"telegram flag on discord": {"--account", "home", "--chat", "-1001234567890"},
		"topic flag on discord":    {"--account", "home", "--topic", "42"},
		"topic without chat":       {"--channel", "telegram", "--account", "pocket", "--topic", "42"},
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

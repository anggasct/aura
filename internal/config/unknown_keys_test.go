package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
)

func TestUnknownKeysAreAllReportedWithLineAndColumn(t *testing.T) {
	content := "version: 1\n" +
		"bogus: 1\n" +
		"channels:\n" +
		"  discord:\n" +
		"    accounts:\n" +
		"      - name: main\n" +
		"        extra: 1\n" +
		"models:\n" +
		"  definitions:\n" +
		"    primary:\n" +
		"      protocol: openai_chat_compat\n" +
		"      model: m\n" +
		"      typo: 1\n" +
		"server:\n" +
		"  port: 8080\n" +
		"server.port: 1\n"
	_, err := Load(writeTempConfig(t, content))
	if err == nil {
		t.Fatal("Load succeeded, want four unknown keys")
	}
	want := []string{
		unknownKeyText("bogus", "line 2, column 1"),
		unknownKeyText("channels.discord.accounts[0].extra", "line 7, column 9"),
		unknownKeyText("models.definitions.primary.typo", "line 13, column 7"),
		unknownKeyText("server.port", "line 16, column 1"),
	}
	if got := strings.Split(strings.TrimPrefix(err.Error(), "config: "), "\n"); !slices.Equal(got, want) {
		t.Errorf("problems = %q, want %q", got, want)
	}
	for _, problem := range fieldProblems(err) {
		if !fault.HasCode(problem, configkit.ErrorCodeUnknownKey) {
			t.Errorf("problem %v is not coded %s", problem, configkit.ErrorCodeUnknownKey)
		}
	}
}

func TestKeyMatchingIsExact(t *testing.T) {
	t.Run("a differently cased key is an unknown key", func(t *testing.T) {
		_, err := Load(writeTempConfig(t, "version: 1\nServer:\n  port: 1\n"))
		wantCode(t, err, unknownKeyCode)
		if !strings.Contains(err.Error(), unknownKeyText("Server", "line 2, column 1")) {
			t.Errorf("error %q does not name Server and its position", err)
		}
	})
	t.Run("the decoder does not fold case", func(t *testing.T) {
		var cfg Config
		err := decodeInput(map[string]any{"Server": map[string]any{"port": 1}}, &cfg)
		if err == nil || !strings.Contains(err.Error(), "Server") {
			t.Fatalf("decodeInput error = %v, want the Server key refused", err)
		}
	})
}

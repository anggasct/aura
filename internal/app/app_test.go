package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/config"
)

const compositionModelsYAML = `models:
  definitions:
    primary:
      protocol: openai_chat_compat
      model: test-model
      api_key_env: AURA_TEST_MODEL_KEY
      capabilities:
        streaming: true
        tools: true
        context_tokens: 200000
        tokenizer: anthropic
`

func restoreDefaultLogger(t *testing.T) {
	t.Helper()
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
}

func writeChatCompositionConfig(t *testing.T, extra string) string {
	t.Helper()
	root := t.TempDir()
	content := fmt.Sprintf("version: 1\nstorage:\n  path: %s\n", filepath.Join(root, "data")) + compositionModelsYAML + extra
	path := filepath.Join(root, "aura.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func loadCompositionConfig(t *testing.T, path string) *config.LoadResult {
	t.Helper()
	result, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return &result
}

func chatCompositionOptions(result *config.LoadResult, logs io.Writer) *Options {
	return &Options{
		Surface: SurfaceChat, Config: result, LogLevel: "debug", LogFormat: "json", LogOutput: logs,
		Chat: ChatOptions{In: strings.NewReader(""), Out: &bytes.Buffer{}, Diag: &bytes.Buffer{}, Plain: true},
	}
}

func lifecycleNames(t *testing.T, logs, message string) []string {
	t.Helper()
	var names []string
	for line := range strings.SplitSeq(logs, "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Msg  string `json:"msg"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if record.Msg == message {
			names = append(names, record.Name)
		}
	}
	return names
}

func requireNames(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func buildApp(t *testing.T, opts *Options) *App {
	t.Helper()
	app, err := Build(t.Context(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return app
}

func TestCompositionRejectsInvalidOptions(t *testing.T) {
	restoreDefaultLogger(t)
	defaults := config.Default()
	chat := func() *Options {
		return &Options{
			Surface: SurfaceChat, Config: &config.LoadResult{Config: &defaults}, LogOutput: io.Discard,
			Chat: ChatOptions{In: strings.NewReader(""), Out: io.Discard, Diag: io.Discard},
		}
	}
	cases := []struct {
		name string
		opts func() *Options
	}{
		{"nil options", func() *Options { return nil }},
		{"nil config result", func() *Options { o := chat(); o.Config = nil; return o }},
		{"nil config", func() *Options { o := chat(); o.Config = &config.LoadResult{}; return o }},
		{"unknown surface", func() *Options { o := chat(); o.Surface = "exec"; return o }},
		{"nil log output", func() *Options { o := chat(); o.LogOutput = nil; return o }},
		{"chat without input", func() *Options { o := chat(); o.Chat.In = nil; return o }},
		{"chat without output", func() *Options { o := chat(); o.Chat.Out = nil; return o }},
		{"chat without diagnostics", func() *Options { o := chat(); o.Chat.Diag = nil; return o }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, err := Build(t.Context(), tc.opts())
			if app != nil {
				t.Fatalf("Build returned an app for invalid options")
			}
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("Build error = %v, want %s", err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestCompositionChatRunsToEndOfInput(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	result := loadCompositionConfig(t, writeChatCompositionConfig(t, ""))
	logs := &syncBuffer{}
	app := buildApp(t, chatCompositionOptions(result, logs))
	if err := app.Run(t.Context()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	stopped := lifecycleNames(t, logs.String(), "lifecycle stopped")
	if len(stopped) == 0 || stopped[len(stopped)-1] != "sqlite" {
		t.Fatalf("lifecycle stopped = %v, want sqlite last", stopped)
	}
}

func TestCompositionChatRegistersEveryConfiguredEntry(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	result := loadCompositionConfig(t, writeChatCompositionConfig(t, "profile:\n  enabled: true\n"))
	app := buildApp(t, chatCompositionOptions(result, &syncBuffer{}))
	requireNames(t, "entries", app.group.Names(), []string{"sqlite", "profile_extraction"})
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close without Run: %v", err)
	}
}

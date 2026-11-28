package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/child"
	"github.com/anggasct/aura/internal/modules/schedule"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
)

const compositionModelsYAML = `models:
  definitions:
    primary:
      protocol: openai_chat_compat
      model: test-model
      api_key_ref: env://AURA_TEST_MODEL_KEY
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

func loadCompositionConfig(t *testing.T, path string) *appconfig.LoadResult {
	t.Helper()
	result, err := appconfig.Load(path)
	if err != nil {
		t.Fatalf("appconfig.Load: %v", err)
	}
	return &result
}

func chatCompositionOptions(result *appconfig.LoadResult, logs io.Writer) *Options {
	return &Options{
		Surface: SurfaceChat, Config: result, LogLevel: "debug", LogFormat: "json", LogOutput: logs,
		Chat: ChatOptions{In: strings.NewReader(""), Out: &bytes.Buffer{}, Diag: &bytes.Buffer{}, Plain: true},
	}
}

func writeCommandCompositionConfig(t *testing.T, definition string) string {
	t.Helper()
	root := t.TempDir()
	definitions := filepath.Join(root, "definitions")
	if err := os.MkdirAll(definitions, 0o700); err != nil {
		t.Fatalf("mkdir definitions: %v", err)
	}
	if err := os.WriteFile(filepath.Join(definitions, "demo.yaml"), []byte(definition), 0o600); err != nil {
		t.Fatalf("write definition: %v", err)
	}
	content := fmt.Sprintf("version: 1\nstorage:\n  path: %s\nworkflows:\n  definitions_dir: %s\n", filepath.Join(root, "data"), definitions) + compositionModelsYAML
	path := filepath.Join(root, "aura.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func commandCompositionOptions(result *appconfig.LoadResult, logs io.Writer, parts ...Part) *Options {
	return &Options{
		Surface: SurfaceCommand, Config: result, LogLevel: "debug", LogFormat: "json", LogOutput: logs,
		Command: CommandOptions{Parts: parts},
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
	defaults := appconfig.Default()
	chat := func() *Options {
		return &Options{
			Surface: SurfaceChat, Config: &appconfig.LoadResult{Config: &defaults}, LogOutput: io.Discard,
			Chat: ChatOptions{In: strings.NewReader(""), Out: io.Discard, Diag: io.Discard},
		}
	}
	command := func() *Options {
		return &Options{Surface: SurfaceCommand, Config: &appconfig.LoadResult{Config: &defaults}, LogOutput: io.Discard}
	}
	cases := []struct {
		name string
		opts func() *Options
	}{
		{"nil options", func() *Options { return nil }},
		{"nil config result", func() *Options { o := chat(); o.Config = nil; return o }},
		{"nil config", func() *Options { o := chat(); o.Config = &appconfig.LoadResult{}; return o }},
		{"unknown surface", func() *Options { o := chat(); o.Surface = "exec"; return o }},
		{"nil log output", func() *Options { o := chat(); o.LogOutput = nil; return o }},
		{"chat without input", func() *Options { o := chat(); o.Chat.In = nil; return o }},
		{"chat without output", func() *Options { o := chat(); o.Chat.Out = nil; return o }},
		{"chat without diagnostics", func() *Options { o := chat(); o.Chat.Diag = nil; return o }},
		{"unknown part", func() *Options { o := command(); o.Command.Parts = []Part{"cache"}; return o }},
		{"duplicate part", func() *Options { o := command(); o.Command.Parts = []Part{PartDurable, PartDurable}; return o }},
		{"parts on the chat surface", func() *Options { o := chat(); o.Command.Parts = []Part{PartDurable}; return o }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, err := Build(t.Context(), tc.opts())
			if app != nil {
				t.Fatalf("Build returned an app for invalid options")
			}
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
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

func TestCompositionCommandBuildsOnlyStorage(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	result := loadCompositionConfig(t, writeCommandCompositionConfig(t, validWorkflowYAML))
	app := buildApp(t, commandCompositionOptions(result, &syncBuffer{}))
	requireNames(t, "entries", app.group.Names(), []string{"sqlite"})
	if app.runtime != nil || app.interpreter != nil || app.models != nil || app.knowledge != nil || app.memory != nil || app.profile != nil || app.plugins != nil {
		t.Fatalf("an unnamed part was built: runtime=%v interpreter=%v models=%v knowledge=%v memory=%v profile=%v plugins=%v", app.runtime, app.interpreter, app.models, app.knowledge, app.memory, app.profile, app.plugins)
	}
	if err := app.Run(t.Context()); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("Run on a command app = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	for range 2 {
		if err := app.Close(t.Context()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

func TestCompositionCommandBuildsNamedParts(t *testing.T) {
	cases := []struct {
		name                                                              string
		parts                                                             []Part
		runtime, interpreter, models, knowledge, memory, profile, plugins bool
	}{
		{"durable", []Part{PartDurable}, true, false, false, false, false, false, false},
		{"workflows", []Part{PartWorkflows}, true, true, false, false, false, false, false},
		{"models", []Part{PartModels}, false, false, true, false, false, false, false},
		{"knowledge", []Part{PartKnowledge}, false, false, false, true, false, false, false},
		{"memory", []Part{PartMemory}, false, false, false, false, true, false, false},
		{"profile", []Part{PartProfile}, false, false, false, false, false, true, false},
		{"plugins", []Part{PartPlugins}, false, false, false, false, false, false, true},
		{"models, workflows, durable, knowledge, memory, profile, plugins", []Part{PartModels, PartWorkflows, PartDurable, PartKnowledge, PartMemory, PartProfile, PartPlugins}, true, true, true, true, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restoreDefaultLogger(t)
			t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
			result := loadCompositionConfig(t, writeCommandCompositionConfig(t, validWorkflowYAML))
			app := buildApp(t, commandCompositionOptions(result, &syncBuffer{}, tc.parts...))
			t.Cleanup(func() { _ = app.Close(context.Background()) })
			if got := app.runtime != nil; got != tc.runtime {
				t.Errorf("durable runtime built = %t, want %t", got, tc.runtime)
			}
			if got := app.interpreter != nil; got != tc.interpreter {
				t.Errorf("workflow interpreter built = %t, want %t", got, tc.interpreter)
			}
			if got := app.models != nil; got != tc.models {
				t.Errorf("model registry built = %t, want %t", got, tc.models)
			}
			if got := app.knowledge != nil; got != tc.knowledge {
				t.Errorf("knowledge service built = %t, want %t", got, tc.knowledge)
			}
			if got := app.memory != nil; got != tc.memory {
				t.Errorf("memory service built = %t, want %t", got, tc.memory)
			}
			if got := app.profile != nil; got != tc.profile {
				t.Errorf("profile service built = %t, want %t", got, tc.profile)
			}
			if got := app.plugins != nil; got != tc.plugins {
				t.Errorf("plugin service built = %t, want %t", got, tc.plugins)
			}
		})
	}
}

func TestRootCommandMountsPluginCommands(t *testing.T) {
	root := NewRootCommand()
	cmd, _, err := root.Find([]string{"plugin"})
	if err != nil || cmd == nil || cmd.Name() != "plugin" {
		t.Fatalf("root.Find(plugin) = %v, %v", cmd, err)
	}
	for _, sub := range []string{"list", "install", "review", "approve", "disable", "remove", "login", "logout"} {
		found, _, err := root.Find([]string{"plugin", sub})
		if err != nil || found == nil || found.Name() != sub {
			t.Fatalf("root.Find(plugin %s) = %v, %v", sub, found, err)
		}
	}
}

func TestRootCommandMountsKnowledgeCommands(t *testing.T) {
	root := NewRootCommand()
	kb, _, err := root.Find([]string{"kb"})
	if err != nil || kb == nil || kb.Name() != "kb" {
		t.Fatalf("root.Find(kb) = %v, %v", kb, err)
	}
	for _, sub := range []string{"add", "list", "show", "remove", "refresh", "embed", "check"} {
		found, _, err := root.Find([]string{"kb", sub})
		if err != nil || found == nil || found.Name() != sub {
			t.Errorf("root.Find(kb %s) = %v, %v", sub, found, err)
		}
	}
}

func TestCompositionCommandWorkflowsAreClientsOfTheServer(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	result := loadCompositionConfig(t, writeCommandCompositionConfig(t, validWorkflowYAML))
	app := buildApp(t, commandCompositionOptions(result, &syncBuffer{}, PartWorkflows))
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	if app.runtime == nil {
		t.Fatal("a workflows-only command build did not build the durable runtime")
	}
	if _, err := app.interpreter.StartConfigured(t.Context(), &workflow.ConfiguredStart{DefinitionID: "demo", Input: nil, PrincipalID: principal.OwnerID, Class: principal.ClassOwner, DurableKey: ""}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := countRows(t, app.db, `SELECT COUNT(*) FROM durable_run WHERE handler = ? AND state = 'running' AND owner IS NULL`, workflow.HandlerName); n != 1 {
		t.Fatalf("unowned running workflow runs = %d, want 1", n)
	}
}

func TestCompositionCommandStartsRunsForTheServer(t *testing.T) {
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	result := loadCompositionConfig(t, writeCommandCompositionConfig(t, validWorkflowYAML))
	app := buildApp(t, commandCompositionOptions(result, &syncBuffer{}, PartDurable))
	t.Cleanup(func() { _ = app.Close(context.Background()) })
	requireNames(t, "entries", app.group.Names(), []string{"sqlite"})
	for _, handler := range []string{broadcast.HandlerName, schedule.HandlerName, child.HandlerName, workflow.HandlerName} {
		t.Run(handler, func(t *testing.T) {
			if _, err := app.runtime.Start(t.Context(), durable.StartRequest{Handler: handler, Key: "run-" + handler}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if n := countRows(t, app.db, `SELECT COUNT(*) FROM durable_run WHERE key = ? AND handler = ? AND state = 'running' AND owner IS NULL`, "run-"+handler, handler); n != 1 {
				t.Fatalf("unowned running rows = %d, want 1", n)
			}
		})
	}
	_, err := app.runtime.Start(t.Context(), durable.StartRequest{Handler: "unknown", Key: "run-unknown"})
	if !fault.HasCode(err, sqlite.ErrorCodeHandlerNotRegistered) {
		t.Fatalf("Start of an undeclared handler = %v, want %s", err, sqlite.ErrorCodeHandlerNotRegistered)
	}
}

func TestCompositionCommandHasNoCapabilityGate(t *testing.T) {
	restoreDefaultLogger(t)
	result := loadCompositionConfig(t, writeCommandCompositionConfig(t, validWorkflowYAML))
	result.CapabilityStateError = errors.New("capability state is unreadable")
	logs := &syncBuffer{}
	app := buildApp(t, commandCompositionOptions(result, logs))
	if err := app.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if line == "" {
			continue
		}
		var record struct {
			Level string `json:"level"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q is not JSON: %v", line, err)
		}
		if record.Level == slog.LevelWarn.String() {
			t.Fatalf("a command build logged a warning: %s", line)
		}
	}
}

func TestCompositionCommandModelOptionsMatchEvaluation(t *testing.T) {
	cfg := appconfig.Default()
	options := evaluationModelOptions(&cfg, sqlitetest.OpenMigrated(t, Schemas()...))
	if options.Prices != nil {
		t.Error("the evaluation registry carries a price registry")
	}
	if options.Vision != (model.VisionWiring{}) {
		t.Error("the evaluation registry carries vision wiring")
	}
	if options.Logger == nil || options.Logger.Handler() != slog.DiscardHandler {
		t.Error("the evaluation registry does not log to the discard handler")
	}
	if options.Checkpoint == nil {
		t.Error("the evaluation registry has no circuit checkpoint")
	}
}

func TestCompositionDurableRuntimeIsBuiltOnce(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		b := newDurableRuntimeBuild()
		b.db = sqlitetest.OpenMigrated(t, Schemas()...)
		first, err := b.durableRuntime()
		if err != nil {
			t.Fatalf("durableRuntime: %v", err)
		}
		second, err := b.durableRuntime()
		if err != nil {
			t.Fatalf("second durableRuntime: %v", err)
		}
		if first != second {
			t.Fatal("durableRuntime built a second runtime")
		}
		if _, isSQLite := first.(*sqlite.Runtime); !isSQLite {
			t.Fatalf("durableRuntime = %T, want the SQLite runtime", first)
		}
	})
	t.Run("storage not open", func(t *testing.T) {
		_, err := newDurableRuntimeBuild().durableRuntime()
		if !fault.HasCode(err, ErrorCodeInvalidArgument) {
			t.Fatalf("durableRuntime without storage = %v, want %s", err, ErrorCodeInvalidArgument)
		}
	})
}

func newDurableRuntimeBuild() *build {
	cfg := appconfig.Default()
	b := newBuild(&Options{Surface: SurfaceCommand, Config: &appconfig.LoadResult{Config: &cfg}})
	b.logger = slog.New(slog.DiscardHandler)
	return b
}

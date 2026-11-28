package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/schedule"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func registryOfPersona(t *testing.T, tools ...string) func() (*auraagent.Registry, error) {
	t.Helper()
	registry, err := auraagent.Build([]auraagent.Persona{{ID: "main", Instructions: "SOUL", Tools: tools}}, definitionNames(), appconfig.BuiltinToolDefinitions(), nil, "en")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return func() (*auraagent.Registry, error) { return registry, nil }
}

func TestWebToolsOfPersonaFollowsTheToolsTheOwnerHas(t *testing.T) {
	for name, tc := range map[string]struct {
		tools []string
		want  bool
	}{
		"one web tool listed": {[]string{"read_file", "web_fetch"}, true},
		"no web tool listed":  {[]string{"read_file"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := webToolsOfPersona(registryOfPersona(t, tc.tools...))(t.Context())
			if err != nil || got != tc.want {
				t.Errorf("offered = %t, %v, want %t", got, err, tc.want)
			}
		})
	}
	t.Run("the built-in persona", func(t *testing.T) {
		registry, err := auraagent.Build(nil, definitionNames(), appconfig.BuiltinToolDefinitions(), nil, "en")
		if err != nil {
			t.Fatal(err)
		}
		offered, err := webToolsOfPersona(func() (*auraagent.Registry, error) { return registry, nil })(t.Context())
		if err != nil || !offered {
			t.Errorf("offered = %t, %v, want the built-in persona to offer a web tool", offered, err)
		}
	})
	t.Run("a registry that cannot be built", func(t *testing.T) {
		failing := func() (*auraagent.Registry, error) { return nil, errors.New("invalid agents") }
		if got, err := webToolsOfPersona(failing)(t.Context()); err == nil || got {
			t.Errorf("offered = %t, %v, want an error and no offer", got, err)
		}
	})
}

func TestScheduledWebOnlyRunFailsClosedWithTheOwnerNotice(t *testing.T) {
	for name, webTools := range map[string]schedule.WebToolsOffered{
		"a persona that offers no web tool": func(context.Context) (bool, error) { return false, nil },
		"a persona that cannot be resolved": func(context.Context) (bool, error) { return false, errors.New("invalid agents") },
	} {
		t.Run(name, func(t *testing.T) {
			db := cronTestDB(t)
			engine := &fakeCronEngine{batches: [][]turn.Event{cronCompletedBatch("turn-1", "done")}}
			runner := &engineTurnRunner{engine: engine, sessions: turnstore.NewSessionService(db), webTools: webTools}
			result, err := runner.RunTurn(t.Context(), &schedule.TurnRequest{
				SessionID: "cron:job-1", PrincipalID: "cron", Origin: "schedule", Prompt: "summarize", Context: schedule.ContextWebOnly,
				IdempotencyKey: "cron:job-1:20260912T210000",
			})
			if err != nil || result.Succeeded || result.SafeErrorCode != string(turn.ErrorCodeWebOnlyUnavailable) || result.TurnID != "" || result.Text == "" {
				t.Fatalf("result = %+v, %v, want a failed result with the owner notice", result, err)
			}
			if len(engine.calls) != 0 {
				t.Errorf("the engine ran %d turns, want none", len(engine.calls))
			}
		})
	}
	t.Run("a persona that offers a web tool, and a full job whatever the persona lists", func(t *testing.T) {
		db := cronTestDB(t)
		engine := &fakeCronEngine{batches: [][]turn.Event{cronCompletedBatch("turn-1", "done"), cronCompletedBatch("turn-2", "done")}}
		runner := &engineTurnRunner{engine: engine, sessions: turnstore.NewSessionService(db), webTools: func(context.Context) (bool, error) { return true, nil }}
		full := &engineTurnRunner{engine: engine, sessions: turnstore.NewSessionService(db), webTools: func(context.Context) (bool, error) { return false, nil }}
		for _, run := range []struct {
			runner  *engineTurnRunner
			context string
		}{{runner, schedule.ContextWebOnly}, {full, schedule.ContextFull}} {
			result, err := run.runner.RunTurn(t.Context(), &schedule.TurnRequest{
				SessionID: "cron:job-1", PrincipalID: "cron", Origin: "schedule", Prompt: "summarize", Context: run.context,
				IdempotencyKey: "cron:job-1:" + run.context,
			})
			if err != nil || !result.Succeeded {
				t.Errorf("%s run = %+v, %v, want it to run", run.context, result, err)
			}
		}
	})
}

func TestCronAddOfAWebOnlyJobNeedsAPersonaWithAWebTool(t *testing.T) {
	for name, tc := range map[string]struct {
		tools   string
		refused bool
	}{
		"a persona that lists a web tool": {tools: "[read_file, web_search]"},
		"a persona that lists none":       {tools: "[read_file]", refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			gf := writeWorkflowFixturesWith(t, "", `agents:
  definitions:
    - id: main
      description: The main persona
      instructions: Help the owner
      tools: `+tc.tools+`
`)
			out, err := runScheduleCommand(t, gf, "add", "--name", "web", "--schedule", "0 3 * * 1", "--timezone", "UTC",
				"--channel", "discord", "--destination", "default", "--prompt", "summarize releases", "--context", "web_only")
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "web_search or web_fetch") {
					t.Fatalf("cron add = %q, %v, want a refusal naming the web tools", out, err)
				}
				return
			}
			if err != nil || !strings.Contains(out, "id: ") {
				t.Fatalf("cron add = %q, %v, want the job scheduled", out, err)
			}
		})
	}
}

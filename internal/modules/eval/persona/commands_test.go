package personaeval

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/store/storetest"
)

func TestCommandsRejectNilOpener(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"eval", "lobby"},
		{"eval-results", "lobby"},
	} {
		t.Run(args[0], func(t *testing.T) {
			t.Parallel()
			root := &cobra.Command{Use: "persona"}
			root.AddCommand(Commands(nil)...)
			buf := &bytes.Buffer{}
			root.SetOut(buf)
			root.SetErr(buf)
			root.SetArgs(args)
			err := root.ExecuteContext(t.Context())
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("args %v with nil opener = %v, want %q", args, err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestScheduleRunnerLogsDistinctCronWarnings(t *testing.T) {
	t.Parallel()
	db := storetest.OpenMigrated(t)
	evalStore, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(&ServiceDeps{Store: evalStore})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := auraagent.Build([]auraagent.Persona{{
		ID: "lobby", Description: "front desk", Instructions: "hi", Audience: auraagent.AudiencePublic,
		Tools: []string{"web_search"}, PublicTools: []string{"web_search"},
		Evals: auraagent.PersonaEvals{Behavior: &auraagent.PersonaEvalBehavior{Schedule: "0 3 * * 1"}},
	}}, []string{"web_search"}, []string{auraagent.DefaultEvalJudgeRoute})
	if err != nil {
		t.Fatal(err)
	}
	definition, _ := registry.Lookup("lobby")

	t.Run("parse_error", func(t *testing.T) {
		t.Parallel()
		var logs bytes.Buffer
		runner, err := NewScheduleRunner(&ScheduleDeps{
			Registry: registry,
			Service:  svc,
			Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
			ParseCron: func(string) (func(loc *time.Location, now time.Time) (time.Time, error), error) {
				return nil, errors.New("bad cron")
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		runner.RunPersona(t.Context(), &definition)
		if !strings.Contains(logs.String(), "persona eval schedule invalid") {
			t.Fatalf("logs = %q, want invalid schedule warning", logs.String())
		}
	})

	t.Run("no_next_fire", func(t *testing.T) {
		t.Parallel()
		var logs bytes.Buffer
		runner, err := NewScheduleRunner(&ScheduleDeps{
			Registry: registry,
			Service:  svc,
			Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
			ParseCron: func(string) (func(loc *time.Location, now time.Time) (time.Time, error), error) {
				return func(*time.Location, time.Time) (time.Time, error) {
					return time.Time{}, errors.New("never fires")
				}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		runner.RunPersona(t.Context(), &definition)
		if !strings.Contains(logs.String(), "persona eval schedule has no next fire") {
			t.Fatalf("logs = %q, want no next fire warning", logs.String())
		}
	})
}

func TestModuleImplementsHealthProviderAndRejectsNilDeps(t *testing.T) {
	t.Parallel()
	if _, err := New(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("New(nil) = %v, want %q", err, ErrorCodeInvalidArgument)
	}
	db := storetest.OpenMigrated(t)
	registry, err := auraagent.Build([]auraagent.Persona{{
		ID: "lobby", Description: "front desk", Instructions: "hi", Audience: auraagent.AudiencePublic,
		Tools: []string{"web_search"}, PublicTools: []string{"web_search"},
	}}, []string{"web_search"}, []string{auraagent.DefaultEvalJudgeRoute})
	if err != nil {
		t.Fatal(err)
	}
	mod, err := New(&ModuleDeps{
		DB:           db,
		Registry:     registry,
		CheckTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var provider health.HealthProvider = mod
	checks := provider.HealthChecks()
	if len(checks) != 1 || checks[0].ID != Component || checks[0].Timeout != 5*time.Second {
		t.Fatalf("HealthChecks = %+v", checks)
	}
	if mod.Gate() == nil || mod.Service() == nil || mod.Store() == nil {
		t.Fatal("module getters must be non-nil")
	}
}

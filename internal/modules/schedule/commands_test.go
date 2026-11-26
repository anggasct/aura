package schedule

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	"github.com/anggasct/aura/internal/platform/fault"
)

func execScheduleCommands(t *testing.T, open Opener, args ...string) (string, error) {
	t.Helper()
	cmds := Commands(open)
	if len(cmds) != 1 {
		t.Fatalf("Commands() returned %d commands, want 1", len(cmds))
	}
	cmd := cmds[0]
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func isUsageError(err error) bool {
	var ue interface{ UsageError() bool }
	return errors.As(err, &ue) && ue.UsageError()
}

func TestScheduleCommandsRefuseNilOpener(t *testing.T) {
	cases := [][]string{
		{"add"},
		{"list"},
		{"pause", "job-1"},
		{"resume", "job-1"},
		{"delete", "job-1"},
		{"runs", "job-1", "--limit", "0"},
		{"run-now", "job-1"},
	}
	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			_, err := execScheduleCommands(t, nil, args...)
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("args %v: err = %v, want invalid_argument", args, err)
			}
		})
	}
}

func TestScheduleCommandsRefuseIncompleteEnv(t *testing.T) {
	nilEnvOpener := func(cmd *cobra.Command, _ bool, fn func(ctx context.Context, env *CommandEnv) error) error {
		return fn(cmd.Context(), nil)
	}
	cases := [][]string{
		{"add"},
		{"list"},
		{"pause", "job-1"},
		{"resume", "job-1"},
		{"delete", "job-1"},
		{"runs", "job-1"},
		{"run-now", "job-1"},
	}
	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			_, err := execScheduleCommands(t, nilEnvOpener, args...)
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("args %v: err = %v, want invalid_argument", args, err)
			}
		})
	}

	t.Run("missing workflow checker", func(t *testing.T) {
		s := mustNewSQLiteStore(t)
		svc, err := NewService(s)
		if err != nil {
			t.Fatalf("NewService(): %v", err)
		}
		opener := func(cmd *cobra.Command, _ bool, fn func(ctx context.Context, env *CommandEnv) error) error {
			return fn(cmd.Context(), &CommandEnv{
				Config:  Default(),
				Store:   s,
				Service: svc,
				Starter: durabletest.NewFake(),
			})
		}
		_, err = execScheduleCommands(t, opener, "add",
			"--name", "n", "--schedule", "0 21 * * *",
			"--channel", "discord", "--destination", "default", "--workflow", "wf-1")
		if !fault.HasCode(err, ErrorCodeInvalidArgument) {
			t.Fatalf("err = %v, want invalid_argument", err)
		}
	})
}

func TestScheduleCommandsUsageErrors(t *testing.T) {
	s := mustNewSQLiteStore(t)
	svc, err := NewService(s)
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	opener := func(cmd *cobra.Command, _ bool, fn func(ctx context.Context, env *CommandEnv) error) error {
		return fn(cmd.Context(), &CommandEnv{
			Config:  Default(),
			Store:   s,
			Service: svc,
			Starter: durabletest.NewFake(),
		})
	}
	cases := []struct {
		name string
		args []string
	}{
		{"runs bad limit", []string{"runs", "job-1", "--limit", "0"}},
		{"runs huge limit", []string{"runs", "job-1", "--limit", "1001"}},
		{"pause missing arg", []string{"pause"}},
		{"add missing flags", []string{"add", "--name", "nightly"}},
		{"add neither prompt nor workflow", []string{"add", "--name", "nightly", "--schedule", "0 21 * * *", "--channel", "discord", "--destination", "default"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execScheduleCommands(t, opener, tc.args...)
			if err == nil || !isUsageError(err) {
				t.Fatalf("args %v: err = %v, want UsageError", tc.args, err)
			}
		})
	}
}

func TestScheduleCommandsListAndRunsFormatting(t *testing.T) {
	s := mustNewSQLiteStore(t)
	svc, err := NewService(s)
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	job := testScheduledJob("job-fmt")
	if err := s.InsertJob(t.Context(), job); err != nil {
		t.Fatalf("InsertJob(): %v", err)
	}
	fire := time.Date(2026, 9, 12, 21, 0, 0, 0, time.UTC)
	if err := s.RecordFire(t.Context(), &Occurrence{
		ID: "occ-fmt", JobID: job.ID, JobVersion: 1, ScheduledForUTC: fire,
		State: OccurrenceCompleted, TurnID: "turn-9", CreatedAt: fire, UpdatedAt: fire,
	}); err != nil {
		t.Fatalf("RecordFire(): %v", err)
	}
	opener := func(cmd *cobra.Command, _ bool, fn func(ctx context.Context, env *CommandEnv) error) error {
		return fn(cmd.Context(), &CommandEnv{Config: Default(), Store: s, Service: svc})
	}

	out, err := execScheduleCommands(t, opener, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"ID\tNAME\tSCHEDULE\tTIMEZONE\tSTATE\tNEXT_FIRE", "job-fmt\tnightly\t0 21 * * *\tUTC\tactive\t"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}

	out, err = execScheduleCommands(t, opener, "runs", job.ID)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	for _, want := range []string{"ID\tSCHEDULED_FOR\tSTATE\tTURN", "occ-fmt\t2026-09-12T21:00:00Z\tcompleted\tturn-9"} {
		if !strings.Contains(out, want) {
			t.Errorf("runs output missing %q:\n%s", want, out)
		}
	}
}

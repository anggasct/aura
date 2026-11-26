package child

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	"github.com/anggasct/aura/internal/platform/fault"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func TestCommandsNilOpenerRejected(t *testing.T) {
	for _, args := range [][]string{{"list"}, {"show", "ch-1"}, {"cancel", "ch-1"}} {
		t.Run(args[0], func(t *testing.T) {
			cmd := Commands(nil)[0]
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(args)
			err := cmd.ExecuteContext(t.Context())
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("Execute(%v) = %v, want %s", args, err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestCommandsInvalidStateReturnsUsageError(t *testing.T) {
	called := false
	cmd := Commands(func(_ *cobra.Command, _ bool, _ func(context.Context, *CommandDeps) error) error {
		called = true
		return nil
	})[0]
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"list", "--state", "bogus"})
	err := cmd.ExecuteContext(t.Context())
	if err == nil || !command.IsUsageError(err) {
		t.Fatalf("err = %v, want usage error", err)
	}
	if called {
		t.Fatal("opener must not run when --state is invalid")
	}
}

func TestCommandsDistinguishStorageOnlyAndDurableOpen(t *testing.T) {
	s, db := newTestStore(t)
	seedChildSessions(t, db, "sess-parent", "sess-child-ch-1")
	if err := s.InsertRun(t.Context(), testChildRun("ch-1")); err != nil {
		t.Fatalf("InsertRun: %v", err)
	}
	registry, err := NewStoreRegistry(s, turnstore.NewSessionService(db))
	if err != nil {
		t.Fatalf("NewStoreRegistry: %v", err)
	}
	runs, err := NewDurableCanceller(durabletest.NewFake())
	if err != nil {
		t.Fatalf("NewDurableCanceller: %v", err)
	}
	canceller, err := NewCancelService(registry, runs)
	if err != nil {
		t.Fatalf("NewCancelService: %v", err)
	}

	var durableFlags []bool
	opener := func(cmd *cobra.Command, needsDurable bool, fn func(context.Context, *CommandDeps) error) error {
		durableFlags = append(durableFlags, needsDurable)
		deps := &CommandDeps{Store: s, Now: func() time.Time { return time.Now().UTC() }}
		if needsDurable {
			deps.Canceller = canceller
		}
		return fn(cmd.Context(), deps)
	}

	run := func(args ...string) (string, error) {
		cmd := Commands(opener)[0]
		out := &bytes.Buffer{}
		cmd.SetOut(out)
		cmd.SetErr(out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}

	if out, err := run("list"); err != nil || !strings.Contains(out, "ch-1") {
		t.Fatalf("list = %q, %v", out, err)
	}
	if out, err := run("show", "ch-1"); err != nil || !strings.Contains(out, "id: ch-1") {
		t.Fatalf("show = %q, %v", out, err)
	}
	if _, err := run("show", "missing"); !fault.HasCode(err, ErrorCodeChildNotFound) {
		t.Fatalf("show missing = %v, want %s", err, ErrorCodeChildNotFound)
	}
	if out, err := run("cancel", "ch-1"); err != nil || !strings.Contains(out, "state=cancelled") {
		t.Fatalf("cancel = %q, %v", out, err)
	}
	wantFlags := []bool{false, false, false, true}
	if len(durableFlags) != len(wantFlags) {
		t.Fatalf("durableFlags = %v, want %v", durableFlags, wantFlags)
	}
	for i := range wantFlags {
		if durableFlags[i] != wantFlags[i] {
			t.Fatalf("durableFlags[%d] = %v, want %v (all: %v)", i, durableFlags[i], wantFlags[i], durableFlags)
		}
	}
}

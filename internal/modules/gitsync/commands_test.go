package gitsync

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestCommandsRejectNilOpener(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"status", "run", "reconcile", "doctor"} {
		t.Run(sub, func(t *testing.T) {
			t.Parallel()
			cmd := Commands(nil)[0]
			buf := &bytes.Buffer{}
			cmd.SetOut(buf)
			cmd.SetErr(buf)
			cmd.SetArgs([]string{sub})
			err := cmd.ExecuteContext(t.Context())
			if err == nil {
				t.Fatalf("%s with nil opener = nil, want error", sub)
			}
			if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("code = %v,%v, want %q", code, ok, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestCommandsRejectNilConfig(t *testing.T) {
	t.Parallel()
	open := func(cmd *cobra.Command, fn func(ctx context.Context, env CommandEnv) error) error {
		return fn(cmd.Context(), CommandEnv{DataRoot: t.TempDir()})
	}
	cmd := Commands(open)[0]
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"status"})
	err := cmd.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("status with nil config = nil, want error")
	}
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeNotConfigured {
		t.Fatalf("code = %v,%v, want %q", code, ok, ErrorCodeNotConfigured)
	}
}

func TestDefaultSyncRunPassOutcomes(t *testing.T) {
	t.Parallel()

	t.Run("nil_config", func(t *testing.T) {
		t.Parallel()
		_, err := defaultSyncRunPass(t.Context(), nil, NewStateStore())
		if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeNotConfigured {
			t.Fatalf("code = %v,%v, want %q", code, ok, ErrorCodeNotConfigured)
		}
	})

	t.Run("invalid_manifest_conflicts", func(t *testing.T) {
		t.Parallel()
		cfg := Default()
		cfg.Remote = "ssh://git@example.com/owner/repo.git"
		cfg.Include = []string{"workspace/**"}
		outcome, err := defaultSyncRunPass(t.Context(), cfg, NewStateStore())
		if err != nil {
			t.Fatalf("defaultSyncRunPass: %v", err)
		}
		if outcome.State != WorkerConflict || !outcome.Conflict || outcome.Result != string(AdvanceConflict) {
			t.Fatalf("outcome = %+v, want conflict", outcome)
		}
	})

	t.Run("invalid_transport_unknown", func(t *testing.T) {
		t.Parallel()
		cfg := Default()
		cfg.Remote = "http://git.example.com/owner/repo.git"
		outcome, err := defaultSyncRunPass(t.Context(), cfg, NewStateStore())
		if err != nil {
			t.Fatalf("defaultSyncRunPass: %v", err)
		}
		if outcome.State != WorkerUnknown || !outcome.Retryable || outcome.Result != string(WorkerUnknown) {
			t.Fatalf("outcome = %+v, want retryable unknown", outcome)
		}
	})

	t.Run("up_to_date_refs", func(t *testing.T) {
		t.Parallel()
		cfg := Default()
		cfg.Remote = "ssh://git@example.com/owner/repo.git"
		store := NewStateStore()
		store.MarkIdle("aaa", "aaa", time.Now().UTC(), time.Millisecond, "ok")
		outcome, err := defaultSyncRunPass(t.Context(), cfg, store)
		if err != nil {
			t.Fatalf("defaultSyncRunPass: %v", err)
		}
		if outcome.State != WorkerIdle || outcome.Result != string(AdvanceUpToDate) {
			t.Fatalf("outcome = %+v, want idle up_to_date", outcome)
		}
	})

	t.Run("fast_forward_refs", func(t *testing.T) {
		t.Parallel()
		cfg := Default()
		cfg.Remote = "ssh://git@example.com/owner/repo.git"
		store := NewStateStore()
		store.MarkIdle("aaa", "bbb", time.Now().UTC(), time.Millisecond, "ok")
		outcome, err := defaultSyncRunPass(t.Context(), cfg, store)
		if err != nil {
			t.Fatalf("defaultSyncRunPass: %v", err)
		}
		if outcome.State != WorkerIdle || outcome.Result != "fast_forwarded" {
			t.Fatalf("outcome = %+v, want idle fast_forwarded", outcome)
		}
	})

	t.Run("decide_advance_or_conflict_empty_ref", func(t *testing.T) {
		t.Parallel()
		if got := decideAdvanceOrConflict(t.Context(), "", "bbb"); got != AdvanceConflict {
			t.Fatalf("decideAdvanceOrConflict = %q, want %q", got, AdvanceConflict)
		}
		if got := decideAdvanceOrConflict(t.Context(), "aaa", "aaa"); got != AdvanceUpToDate {
			t.Fatalf("decideAdvanceOrConflict = %q, want %q", got, AdvanceUpToDate)
		}
		if got := decideAdvanceOrConflict(t.Context(), "aaa", "bbb"); got != AdvanceFastForward {
			t.Fatalf("decideAdvanceOrConflict = %q, want %q", got, AdvanceFastForward)
		}
	})
}

func TestDoctorRedactsFaultDetail(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Remote = "http://git.example.com/owner/repo.git"
	open := func(cmd *cobra.Command, fn func(ctx context.Context, env CommandEnv) error) error {
		return fn(cmd.Context(), CommandEnv{Config: cfg, DataRoot: t.TempDir()})
	}
	cmd := Commands(open)[0]
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"doctor"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !strings.Contains(buf.String(), "transport: invalid: remote scheme must be ssh or https") {
		t.Fatalf("doctor output = %q, want transport invalid reason", buf.String())
	}
}

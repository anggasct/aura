package broadcast

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
)

func execBroadcastCommands(t *testing.T, open Opener, args ...string) (string, error) {
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

type fakeEffectProvider struct{}

func (fakeEffectProvider) SupportsIdempotency() bool { return false }

func (fakeEffectProvider) Invoke(context.Context, *effect.Invocation) (effect.Outcome, error) {
	return effect.Outcome{}, nil
}

type fakeEffectReconciler struct {
	intent    *effect.Intent
	resolved  *effect.Intent
	reconcile error
}

func (f *fakeEffectReconciler) GetIntent(context.Context, string) (*effect.Intent, error) {
	return f.intent, nil
}

func (f *fakeEffectReconciler) Reconcile(context.Context, string, effect.Provider) (*effect.Intent, error) {
	return f.resolved, f.reconcile
}

func seedEffectIntentRow(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `INSERT OR IGNORE INTO session (id, owner_id, metadata_json, created_at, updated_at) VALUES ('sess-reconcile','broadcast','{}','2026-09-12T12:00:00Z','2026-09-12T12:00:00Z')`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO effect_intent (id, session_id, turn_id, tool_call_id, idempotency_key, provider, operation, classification, state, request_digest, request_json, prepared_at, updated_at) VALUES (?, 'sess-reconcile', '', '', ?, 'discord', 'send_message', 'effectful', 'unknown', 'digest', '{}', '2026-09-12T12:00:00Z', '2026-09-12T12:00:00Z')`, id, "key-"+id); err != nil {
		t.Fatalf("seed effect_intent: %v", err)
	}
}

func TestCommandsRefusesNilOpener(t *testing.T) {
	cases := [][]string{
		{"status", "--limit", "0"},
		{"reconcile", "bcst-1"},
	}
	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			_, err := execBroadcastCommands(t, nil, args...)
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("args %v: err = %v, want invalid_argument", args, err)
			}
		})
	}
}

func TestBroadcastCommandsRefuseIncompleteEnv(t *testing.T) {
	nilEnvOpener := func(cmd *cobra.Command, _ bool, fn func(ctx context.Context, env *CommandEnv) error) error {
		return fn(cmd.Context(), nil)
	}
	for _, args := range [][]string{{"status"}, {"reconcile", "bcst-1"}} {
		t.Run(args[0], func(t *testing.T) {
			_, err := execBroadcastCommands(t, nilEnvOpener, args...)
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("args %v: err = %v, want invalid_argument", args, err)
			}
		})
	}

	t.Run("missing effect reconciler on linked unknown", func(t *testing.T) {
		db, s := mustNewSQLiteStore(t)
		seedEffectIntentRow(t, db, "eff-1")
		item := testBroadcastItem("bcst-unk", "cron", "key-unk")
		item.State = StateUnknown
		item.EffectID = "eff-1"
		if _, _, err := s.Insert(t.Context(), item); err != nil {
			t.Fatalf("Insert(): %v", err)
		}
		opener := func(cmd *cobra.Command, _ bool, fn func(ctx context.Context, env *CommandEnv) error) error {
			return fn(cmd.Context(), &CommandEnv{Store: s, Runtime: durabletest.NewFake()})
		}
		_, err := execBroadcastCommands(t, opener, "reconcile", "bcst-unk")
		if !fault.HasCode(err, ErrorCodeInvalidArgument) {
			t.Fatalf("err = %v, want invalid_argument", err)
		}
	})
}

func TestBroadcastCommandsUsageErrors(t *testing.T) {
	_, s := mustNewSQLiteStore(t)
	opener := func(cmd *cobra.Command, _ bool, fn func(ctx context.Context, env *CommandEnv) error) error {
		return fn(cmd.Context(), &CommandEnv{Store: s, Runtime: durabletest.NewFake()})
	}
	cases := []struct {
		name string
		args []string
	}{
		{"status zero limit", []string{"status", "--limit", "0"}},
		{"status huge limit", []string{"status", "--limit", "1001"}},
		{"reconcile missing arg", []string{"reconcile"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := execBroadcastCommands(t, opener, tc.args...)
			if err == nil || !isUsageError(err) {
				t.Fatalf("args %v: err = %v, want UsageError", tc.args, err)
			}
		})
	}
}

func TestBroadcastCommandsStatusContentVisibility(t *testing.T) {
	_, s := mustNewSQLiteStore(t)
	item := testBroadcastItem("bcst-vis", "cron", "key-vis")
	item.ContentJSON = `{"text":"hidden-secret"}`
	if _, _, err := s.Insert(t.Context(), item); err != nil {
		t.Fatalf("Insert(): %v", err)
	}
	opener := func(cmd *cobra.Command, _ bool, fn func(ctx context.Context, env *CommandEnv) error) error {
		return fn(cmd.Context(), &CommandEnv{Store: s})
	}

	out, err := execBroadcastCommands(t, opener, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(out, "hidden-secret") {
		t.Fatalf("status leaked content by default:\n%s", out)
	}
	out, err = execBroadcastCommands(t, opener, "status", "--show-content")
	if err != nil {
		t.Fatalf("status --show-content: %v", err)
	}
	if !strings.Contains(out, "hidden-secret") {
		t.Fatalf("status --show-content hid content:\n%s", out)
	}
}

func TestBroadcastReconcileSettlesResolvedEffect(t *testing.T) {
	cases := []struct {
		name         string
		resolved     *effect.Intent
		reconcileErr error
		wantOutput   string
		wantState    string
	}{
		{
			name:       "succeeded",
			resolved:   &effect.Intent{ID: "eff-1", Provider: "discord", State: effect.StateSucceeded},
			wantOutput: "result: settled succeeded",
			wantState:  StateSucceeded,
		},
		{
			name:       "failed",
			resolved:   &effect.Intent{ID: "eff-1", Provider: "discord", State: effect.StateFailed},
			wantOutput: "result: settled failed",
			wantState:  StateFailed,
		},
		{
			name:       "still unknown",
			resolved:   &effect.Intent{ID: "eff-1", Provider: "discord", State: effect.StateUnknown},
			wantOutput: "result: unchanged (effect still unknown)",
			wantState:  StateUnknown,
		},
		{
			name:         "inconclusive error",
			reconcileErr: errors.New("network timeout"),
			wantOutput:   "result: unchanged (reconciliation inconclusive)",
			wantState:    StateUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, s := mustNewSQLiteStore(t)
			seedEffectIntentRow(t, db, "eff-1")
			item := testBroadcastItem("bcst-1", "cron", "key-1")
			item.State = StateUnknown
			item.EffectID = "eff-1"
			if _, _, err := s.Insert(t.Context(), item); err != nil {
				t.Fatalf("Insert(): %v", err)
			}
			reconciler := &fakeEffectReconciler{
				intent:    &effect.Intent{ID: "eff-1", Provider: "discord", State: effect.StateUnknown},
				resolved:  tc.resolved,
				reconcile: tc.reconcileErr,
			}
			opener := func(cmd *cobra.Command, _ bool, fn func(ctx context.Context, env *CommandEnv) error) error {
				return fn(cmd.Context(), &CommandEnv{
					Store:   s,
					Runtime: durabletest.NewFake(),
					Effects: func() (EffectReconciler, error) { return reconciler, nil },
					EffectProviders: func() (map[string]effect.Provider, error) {
						return map[string]effect.Provider{"discord": fakeEffectProvider{}}, nil
					},
				})
			}

			out, err := execBroadcastCommands(t, opener, "reconcile", "bcst-1")
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if !strings.Contains(out, tc.wantOutput) {
				t.Errorf("output = %q, want %q", out, tc.wantOutput)
			}
			stored, err := s.Item(t.Context(), "bcst-1")
			if err != nil {
				t.Fatalf("Item(): %v", err)
			}
			if stored.State != tc.wantState {
				t.Errorf("stored state = %q, want %q", stored.State, tc.wantState)
			}
		})
	}
}

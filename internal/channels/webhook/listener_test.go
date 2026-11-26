package webhook

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	platformwebhook "github.com/anggasct/aura/internal/platform/webhook"
	"github.com/anggasct/aura/internal/store/storetest"
)

func validChannelDeps(t *testing.T) *Deps {
	t.Helper()
	db := storetest.OpenMigrated(t)
	executions, err := platformwebhook.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	cfg := platformwebhook.Default()
	cfg.Enabled = true
	cfg.Listen = "127.0.0.1:0"
	cfg.Keys = []platformwebhook.Key{{ID: "primary", SecretEnv: "TEST_WEBHOOK_SECRET"}}
	return &Deps{
		Config:     &cfg,
		Sessions:   turnstore.NewSessionService(db),
		Executions: executions,
		Events:     turnstore.NewTurnStore(db),
		Runtime:    &fakeTurnRuntime{events: acceptedTurnEvents},
		Keys:       func(string) (string, error) { return "secret-0123456789abcdef", nil },
		Logger:     slog.New(slog.DiscardHandler),
	}
}

func TestWebhookNewRefusesMissingRequiredDeps(t *testing.T) {
	cases := map[string]struct {
		mutate     func(*Deps) *Deps
		noKeyReads bool
	}{
		"nil deps":            {mutate: func(*Deps) *Deps { return nil }, noKeyReads: true},
		"nil config":          {mutate: func(d *Deps) *Deps { d.Config = nil; return d }, noKeyReads: true},
		"empty listen":        {mutate: func(d *Deps) *Deps { d.Config.Listen = ""; return d }, noKeyReads: true},
		"zero retention":      {mutate: func(d *Deps) *Deps { d.Config.ReplayRetention = 0; return d }, noKeyReads: true},
		"negative retention":  {mutate: func(d *Deps) *Deps { d.Config.ReplayRetention = configkit.Duration(-time.Second); return d }, noKeyReads: true},
		"nil sessions":        {mutate: func(d *Deps) *Deps { d.Sessions = nil; return d }, noKeyReads: true},
		"nil executions":      {mutate: func(d *Deps) *Deps { d.Executions = nil; return d }, noKeyReads: true},
		"nil events":          {mutate: func(d *Deps) *Deps { d.Events = nil; return d }, noKeyReads: true},
		"nil runtime":         {mutate: func(d *Deps) *Deps { d.Runtime = nil; return d }, noKeyReads: true},
		"nil key source":      {mutate: func(d *Deps) *Deps { d.Keys = nil; return d }, noKeyReads: true},
		"invalid acceptuntil": {mutate: func(d *Deps) *Deps { d.Config.Keys[0].AcceptUntil = "not-rfc3339"; return d }, noKeyReads: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			keyReads := 0
			deps := validChannelDeps(t)
			origKeys := deps.Keys
			deps.Keys = func(envName string) (string, error) {
				keyReads++
				return origKeys(envName)
			}
			ch, err := New(tc.mutate(deps))
			if !fault.HasCode(err, platformwebhook.ErrorCodeInvalidArgument) || ch != nil {
				t.Fatalf("New = %v, %v; want %s", ch, err, platformwebhook.ErrorCodeInvalidArgument)
			}
			if tc.noKeyReads && keyReads != 0 {
				t.Fatalf("New read %d secrets despite invalid required deps", keyReads)
			}
		})
	}
}

func TestExecutionStatusStore(t *testing.T) {
	if _, err := NewExecutionStatusStore(nil); !fault.HasCode(err, platformwebhook.ErrorCodeInvalidArgument) {
		t.Fatalf("NewExecutionStatusStore(nil) = %v, want %s", err, platformwebhook.ErrorCodeInvalidArgument)
	}
	db := storetest.OpenMigrated(t)
	executions, err := platformwebhook.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	store, err := NewExecutionStatusStore(executions)
	if err != nil {
		t.Fatalf("NewExecutionStatusStore: %v", err)
	}
	if _, err := store.ExecutionStatus(t.Context(), "whex_missing"); !fault.HasCode(err, platformwebhook.ErrorCodeExecutionNotFound) {
		t.Fatalf("missing execution error = %v, want %s", err, platformwebhook.ErrorCodeExecutionNotFound)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := executions.InsertExecution(t.Context(), &platformwebhook.Execution{
		ID: "whex_status", KeyID: "primary", EventID: "evt-status", Nonce: "nonce-status", BodyDigest: "digest-status",
		TurnID: "turn_status", SessionID: "sess_status", State: platformwebhook.StateAccepted,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("InsertExecution: %v", err)
	}
	got, err := store.ExecutionStatus(t.Context(), "whex_status")
	if err != nil {
		t.Fatalf("ExecutionStatus: %v", err)
	}
	if got.ExecutionID != "whex_status" || got.TurnID != "turn_status" || got.State != platformwebhook.StateAccepted {
		t.Fatalf("status = %+v", got)
	}
}

func TestEnvKeySource(t *testing.T) {
	if _, err := EnvKeySource("AURA_WEBHOOK_UNSET_KEY_TEST"); err == nil {
		t.Fatal("EnvKeySource(unset) = nil, want error")
	}
	t.Setenv("AURA_WEBHOOK_EMPTY_KEY_TEST", "")
	if _, err := EnvKeySource("AURA_WEBHOOK_EMPTY_KEY_TEST"); err == nil {
		t.Fatal("EnvKeySource(empty) = nil, want error")
	}
	t.Setenv("AURA_WEBHOOK_VALID_KEY_TEST", "secret-val")
	got, err := EnvKeySource("AURA_WEBHOOK_VALID_KEY_TEST")
	if err != nil || got != "secret-val" {
		t.Fatalf("EnvKeySource(valid) = %q, %v; want secret-val", got, err)
	}
}

func TestChannelHandlerAndListener(t *testing.T) {
	ch, err := New(validChannelDeps(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if ch.Listener() == nil || ch.Listener().Name() != "webhook" {
		t.Fatalf("Listener = %v", ch.Listener())
	}
	if ch.Handler() == nil {
		t.Fatal("Handler = nil")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/unknown", http.NoBody)
	ch.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /unknown = %d, want 404", rec.Code)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	ch.RunReconcile(ctx)
}

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/modules/broadcast"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func writeBroadcastCLIConfig(t *testing.T, dataDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 1\nstorage:\n  path: " + dataDir + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func seedBroadcastCLIItem(t *testing.T, dbPath, id, state, content string) {
	t.Helper()
	if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
		storetest.Seed(t, dbPath)
	}
	db, err := store.OpenDB(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	now := "2026-09-12T12:00:00Z"
	if _, err := db.ExecContext(t.Context(), `INSERT INTO broadcast_item (id, producer, idempotency_key, content_digest, priority, destination_alias, content_json, state, not_before, attempt_count, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, "cron", "key-"+id, "digest-"+id, "info", "default", content, state, now, 0, now, now); err != nil {
		t.Fatalf("seed item: %v", err)
	}
}

func runBroadcastCommand(t *testing.T, gf *globalFlags, args ...string) (string, error) {
	t.Helper()
	restoreDefaultLogger(t)
	cmd := newBroadcastCmd(gf)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestBroadcastStatusHidesContentByDefault(t *testing.T) {
	dataDir := t.TempDir()
	seedBroadcastCLIItem(t, filepath.Join(dataDir, "aura.db"), "bcst-1", "scheduled", `{"text":"secret plans"}`)
	gf := &globalFlags{configPath: writeBroadcastCLIConfig(t, dataDir)}

	out, err := runBroadcastCommand(t, gf, "status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{"bcst-1", "scheduled", "info", "default"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret plans") {
		t.Errorf("status leaks content by default:\n%s", out)
	}

	out, err = runBroadcastCommand(t, gf, "status", "--show-content")
	if err != nil {
		t.Fatalf("status --show-content: %v", err)
	}
	if !strings.Contains(out, "secret plans") {
		t.Errorf("explicit content flag hides content:\n%s", out)
	}
}

func TestBroadcastStatusFiltersState(t *testing.T) {
	dataDir := t.TempDir()
	seedBroadcastCLIItem(t, filepath.Join(dataDir, "aura.db"), "bcst-1", "scheduled", `{"text":"a"}`)
	seedBroadcastCLIItem(t, filepath.Join(dataDir, "aura.db"), "bcst-2", "held", `{"text":"b"}`)
	gf := &globalFlags{configPath: writeBroadcastCLIConfig(t, dataDir)}

	out, err := runBroadcastCommand(t, gf, "status", "--state", "held")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "bcst-2") || strings.Contains(out, "bcst-1") {
		t.Errorf("state filter wrong:\n%s", out)
	}
	if _, err := runBroadcastCommand(t, gf, "status", "--state", "flying"); err == nil {
		t.Error("invalid state accepted")
	}
}

func TestBroadcastReconcileReportsNoop(t *testing.T) {
	dataDir := t.TempDir()
	seedBroadcastCLIItem(t, filepath.Join(dataDir, "aura.db"), "bcst-1", "scheduled", `{"text":"a"}`)
	seedBroadcastCLIItem(t, filepath.Join(dataDir, "aura.db"), "bcst-9", "unknown", `{"text":"b"}`)
	gf := &globalFlags{configPath: writeBroadcastCLIConfig(t, dataDir)}

	out, err := runBroadcastCommand(t, gf, "reconcile", "bcst-9")
	if err != nil {
		t.Fatalf("reconcile unknown without effect: %v", err)
	}
	if !strings.Contains(out, "unchanged") {
		t.Errorf("reconcile output wrong:\n%s", out)
	}
	if _, err := runBroadcastCommand(t, gf, "reconcile", "missing"); err == nil {
		t.Error("missing item accepted")
	}
}

func TestBroadcastReconcileRestartsAnEndedRun(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "aura.db")
	seedBroadcastCLIItem(t, dbPath, "bcst-1", "scheduled", `{"text":"a"}`)
	db := openDatabaseFile(t, dbPath)
	now := "2026-09-12T12:00:00Z"
	if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_run (key, handler, payload, state, claim, created_at, updated_at) VALUES (?, ?, ?, 'failed', 3, ?, ?)`,
		"bcst-1", broadcast.HandlerName, []byte("bcst-1"), now, now); err != nil {
		t.Fatalf("seed the ended run: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO durable_action (run_key, action_key, outcome, result, created_at) VALUES (?, ?, 'result', ?, ?)`,
		"bcst-1", "send", []byte("{}"), now); err != nil {
		t.Fatalf("seed the journal: %v", err)
	}
	gf := &globalFlags{configPath: writeBroadcastCLIConfig(t, dataDir)}

	out, err := runBroadcastCommand(t, gf, "reconcile", "bcst-1")
	if err != nil {
		t.Fatalf("reconcile: %v\n%s", err, out)
	}
	if !strings.Contains(out, "result: run started") {
		t.Errorf("reconcile output wrong:\n%s", out)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE key = 'bcst-1' AND state = 'running' AND claim = 3`); n != 1 {
		t.Fatalf("running rows with the claim counter kept = %d, want 1", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM durable_action WHERE run_key = 'bcst-1'`); n != 0 {
		t.Fatalf("journal rows = %d, want 0", n)
	}
}

func TestDiscordBroadcastSenderReportsAStartedIntentAsAmbiguous(t *testing.T) {
	cases := []struct {
		name      string
		finish    func(ctx context.Context, journal *effect.Journal, id string) (*effect.Intent, error)
		succeeded bool
		ambiguous bool
	}{
		{name: "an intent an interrupted call left started", ambiguous: true},
		{name: "a failed intent", finish: func(ctx context.Context, journal *effect.Journal, id string) (*effect.Intent, error) {
			return journal.Fail(ctx, id, "discord_rejected")
		}},
		{name: "a succeeded intent", succeeded: true, finish: func(ctx context.Context, journal *effect.Journal, id string) (*effect.Intent, error) {
			return journal.Succeed(ctx, id, json.RawMessage(`{"message_id":"m1"}`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := storetest.OpenMigrated(t)
			sessions := store.NewSessionService(db)
			now := time.Now().UTC()
			if err := sessions.Create(t.Context(), &store.Session{ID: "broadcast:default", OwnerID: "broadcast", Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatalf("Create session: %v", err)
			}
			effects, err := toolsbuiltin.NewChannelEffects(db, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("NewChannelEffects: %v", err)
			}
			payload, err := json.Marshal(discord.SendOperation{ChannelID: "123456", Text: "hello"})
			if err != nil {
				t.Fatalf("encode the request: %v", err)
			}
			prepared, err := effects.Journal().Prepare(t.Context(), &effect.PrepareRequest{
				SessionID:      "broadcast:default",
				IdempotencyKey: "bcst:bcst-1:a1",
				Provider:       "discord",
				Operation:      discord.OpSendMessage,
				Classification: effect.ClassificationEffectful,
				Request:        payload,
				EventKind:      effect.EventKindChannelRequested,
			})
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if _, err := effects.Journal().Start(t.Context(), prepared.ID); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if tc.finish != nil {
				if _, err := tc.finish(t.Context(), effects.Journal(), prepared.ID); err != nil {
					t.Fatalf("finish the intent: %v", err)
				}
			}
			before, err := effects.Journal().Get(t.Context(), prepared.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}

			sender := &discordBroadcastSender{adapter: &discord.Adapter{}, effects: effects, sessions: sessions}
			outcome, err := sender.Send(t.Context(), &broadcast.SendRequest{
				Route:          broadcast.Route{Source: "discord", Instance: "123456"},
				Text:           "hello",
				SessionID:      "broadcast:default",
				IdempotencyKey: "bcst:bcst-1:a1",
			})
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if outcome.IntentID != prepared.ID || outcome.Succeeded != tc.succeeded || outcome.Ambiguous != tc.ambiguous {
				t.Errorf("outcome = %+v, want intent %s, succeeded %t, ambiguous %t", outcome, prepared.ID, tc.succeeded, tc.ambiguous)
			}
			after, err := effects.Journal().Get(t.Context(), prepared.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if after.State != before.State || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Errorf("intent after the send = %s at %s, want %s at %s, not invoked", after.State, after.UpdatedAt, before.State, before.UpdatedAt)
			}
		})
	}
}

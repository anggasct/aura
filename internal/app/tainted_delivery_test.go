package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/media"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/modules/broadcast"
	"github.com/anggasct/aura/internal/modules/schedule"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/turn"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
	runtimeengine "github.com/anggasct/aura/internal/platform/turn/engine"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/platform/turn/turntest"
)

type capturedHTTPRequest struct {
	method string
	path   string
	body   map[string]any
}

type recordingTransport struct {
	mu       sync.Mutex
	requests []capturedHTTPRequest
	nextID   int64
	respond  func(r *http.Request) (int, string)
}

func (rt *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	rt.mu.Lock()
	rt.nextID++
	id := rt.nextID
	rt.requests = append(rt.requests, capturedHTTPRequest{method: r.Method, path: r.URL.Path, body: decoded})
	respond := rt.respond
	rt.mu.Unlock()

	status := http.StatusOK
	respBody := `{"id":"` + strconv.FormatInt(id, 10) + `"}`
	if respond != nil {
		status, respBody = respond(r)
	} else if strings.Contains(r.URL.Path, "/sendMessage") {
		respBody = `{"ok":true,"result":{"message_id":` + strconv.FormatInt(id, 10) + `}}`
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(respBody)),
		Request:    r,
	}, nil
}

func (rt *recordingTransport) messages(suffix string) []capturedHTTPRequest {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var out []capturedHTTPRequest
	for _, req := range rt.requests {
		if strings.HasSuffix(req.path, suffix) {
			out = append(out, req)
		}
	}
	return out
}

func (rt *recordingTransport) reset() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.requests = nil
}

func newOptedInTelegramAdapter(t *testing.T, db *sql.DB, effects *effect.Executor, transport http.RoundTripper) *telegram.Adapter {
	t.Helper()
	t.Setenv("AURA_TEST_TG_PREVIEW_TOKEN", "test-tg-token")
	cfg := telegram.Default()
	cfg.Enabled = true
	account := telegram.Account{
		Name:           "pocket",
		BotTokenRef:    secrettest.Ref("env://AURA_TEST_TG_PREVIEW_TOKEN"),
		LinkPreviews:   true,
		AllowedUserIDs: []string{"100"},
	}
	cfg.Accounts = []telegram.Account{account}
	cursors, err := telegram.NewSQLiteCursorStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteCursorStore: %v", err)
	}
	ensurer, err := access.NewSessionEnsurer(turnstore.NewSessionService(db))
	if err != nil {
		t.Fatalf("NewSessionEnsurer: %v", err)
	}
	adapter, err := telegram.New(&telegram.Deps{
		Config:    &cfg,
		Account:   &account,
		Cursors:   cursors,
		Effects:   effects,
		Sessions:  ensurer,
		Logger:    slog.New(slog.DiscardHandler),
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("telegram.New: %v", err)
	}
	return adapter
}

func newOptedInDiscordAdapter(t *testing.T, db *sql.DB, effects *effect.Executor, transport http.RoundTripper) *discord.Adapter {
	t.Helper()
	t.Setenv("AURA_TEST_DC_PREVIEW_TOKEN", "test-dc-token")
	cfg := discord.Default()
	cfg.Enabled = true
	account := discord.Account{
		Name:           "home",
		BotTokenRef:    secrettest.Ref("env://AURA_TEST_DC_PREVIEW_TOKEN"),
		LinkPreviews:   true,
		AllowedUserIDs: []string{"100"},
	}
	cfg.Accounts = []discord.Account{account}
	resumes, err := discord.NewSQLiteResumeStore(db, account.Name)
	if err != nil {
		t.Fatalf("NewSQLiteResumeStore: %v", err)
	}
	mediaStore, err := media.NewArtifactStore(storage.NewArtifactStore(db, filepath.Join(t.TempDir(), "artifacts"), 1<<20))
	if err != nil {
		t.Fatalf("NewArtifactStore: %v", err)
	}
	ensurer, err := access.NewSessionEnsurer(turnstore.NewSessionService(db))
	if err != nil {
		t.Fatalf("NewSessionEnsurer: %v", err)
	}
	adapter, err := discord.New(&discord.Deps{
		Config:    &cfg,
		Account:   &account,
		Resumes:   resumes,
		Effects:   effects,
		Media:     mediaStore,
		Sessions:  ensurer,
		Logger:    slog.New(slog.DiscardHandler),
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("discord.New: %v", err)
	}
	return adapter
}

func discordEmbedsSuppressed(body map[string]any) bool {
	flags, ok := body["flags"].(float64)
	return ok && flags == 4
}

func telegramPreviewsDisabled(body map[string]any) bool {
	options, ok := body["link_preview_options"].(map[string]any)
	if !ok {
		return false
	}
	disabled, _ := options["is_disabled"].(bool)
	return disabled
}

func TestBroadcastTaintedMigrationDefaultsExistingRowsCleanAndConstrains(t *testing.T) {
	ctx := t.Context()
	history := Migrations()
	step := slices.IndexFunc(history, func(m sqlite.Migration) bool { return m.Owner == "broadcast" && m.Version == 30 })
	if step < 0 {
		t.Fatal("broadcast tainted step 30 is missing from Migrations()")
	}
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "broadcast-v30.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db, history[:step]); err != nil {
		t.Fatalf("Migrate before step 30: %v", err)
	}
	now := "2026-11-28T12:00:00Z"
	if _, err := db.ExecContext(ctx, `INSERT INTO broadcast_item
		(id, producer, idempotency_key, content_digest, priority, destination_alias, content_json, state, not_before, attempt_count, created_at, updated_at)
		VALUES ('bcst-pre', 'cron', 'key-pre', 'digest-pre', 'info', 'default', '{"text":"pre"}', 'scheduled', ?, 0, ?, ?)`,
		now, now, now); err != nil {
		t.Fatalf("seed pre-migration row: %v", err)
	}
	if err := sqlite.Migrate(ctx, db, history); err != nil {
		t.Fatalf("Migrate through step 30: %v", err)
	}
	var tainted int
	if err := db.QueryRowContext(ctx, `SELECT tainted FROM broadcast_item WHERE id = 'bcst-pre'`).Scan(&tainted); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if tainted != 0 {
		t.Errorf("existing row tainted = %d, want 0", tainted)
	}
	if _, err := db.ExecContext(ctx, `UPDATE broadcast_item SET tainted = 2 WHERE id = 'bcst-pre'`); !fault.HasCode(sqlite.Classify(err), sqlite.ErrorCodeConstraintCheck) {
		t.Errorf("out-of-range tainted err = %v, want check constraint", err)
	}
}

func TestTaintedDeliveryNeverShowsLinkPreviews(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schemas()...)
	sessions := turnstore.NewSessionService(db)
	effects, err := newChannelEffects(db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newChannelEffects: %v", err)
	}
	tgTransport := &recordingTransport{}
	dcTransport := &recordingTransport{}
	tgAdapter := newOptedInTelegramAdapter(t, db, effects, tgTransport)
	dcAdapter := newOptedInDiscordAdapter(t, db, effects, dcTransport)

	const linkReply = "see https://evil.example/?d=secret"
	executor := turntest.NewFakeExecutor([]turntest.FakeStep{
		{Kind: turn.EventKindMessageCompleted, Payload: json.RawMessage(`{"text":"` + linkReply + `"}`)},
	})
	engine, err := runtimeengine.New(&runtimeengine.Deps{
		Events:     turnstore.NewEventStore(db),
		Dedupe:     turnstore.NewDedupeStore(db),
		Executor:   executor,
		Config:     runtimeengine.Default(),
		Channels:   []runtimechannelhost.ChannelPort{tgAdapter, dcAdapter},
		LiveEvents: turn.NewEventBus(nil),
	})
	if err != nil {
		t.Fatalf("runtimeengine.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := engine.Shutdown(ctx); err != nil {
			t.Errorf("engine Shutdown: %v", err)
		}
	})

	runTurn := func(t *testing.T, req *turn.TurnRequest) {
		t.Helper()
		now := time.Now().UTC()
		if err := sessions.Create(t.Context(), &turn.Session{
			ID: req.SessionID, OwnerID: req.PrincipalID, Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create session %s: %v", req.SessionID, err)
		}
		for _, err := range engine.Run(t.Context(), req) {
			if err != nil {
				t.Fatalf("engine.Run(%s): %v", req.SessionID, err)
			}
		}
	}

	tgReply := json.RawMessage(`{"chat_id":100,"message_id":1,"chat_type":"private","from_id":100}`)
	dcReply := json.RawMessage(`{"channel_id":"333","message_id":"1","author_id":"100"}`)

	cases := []struct {
		name        string
		mutate      func(*turn.TurnRequest)
		wantTainted bool
	}{
		{name: "clean owner turn keeps previews", wantTainted: false},
		{name: "knowledge scope disables previews", mutate: func(r *turn.TurnRequest) { r.Knowledge = []string{"notes"} }, wantTainted: true},
		{name: "web-only turn disables previews", mutate: func(r *turn.TurnRequest) { r.WebOnly = true; r.JobID = "job-web" }, wantTainted: true},
		{name: "tainted job turn disables previews", mutate: func(r *turn.TurnRequest) { r.JobTaint = "a webhook"; r.JobID = "job-t" }, wantTainted: true},
		{name: "workflow tainted input disables previews", mutate: func(r *turn.TurnRequest) { r.InputTainted = true }, wantTainted: true},
	}

	for i, tc := range cases {
		t.Run("telegram/"+tc.name, func(t *testing.T) {
			tgTransport.reset()
			req := &turn.TurnRequest{
				SessionID:      "main/telegram:pocket/chat:tg-" + strconv.Itoa(i),
				PrincipalID:    principal.OwnerID,
				PrincipalClass: principal.ClassOwner,
				Origin:         turn.Origin("telegram:pocket"),
				Parts:          []runtimeingress.InputPart{{Text: "hello"}},
				ReplyContext:   tgReply,
				IdempotencyKey: "tg-preview-" + strconv.Itoa(i),
			}
			if tc.mutate != nil {
				tc.mutate(req)
			}
			runTurn(t, req)
			reqs := tgTransport.messages("/sendMessage")
			if len(reqs) != 1 {
				t.Fatalf("telegram sends = %d, want 1", len(reqs))
			}
			disabled := telegramPreviewsDisabled(reqs[0].body)
			if tc.wantTainted && !disabled {
				t.Errorf("tainted telegram delivery kept previews: %v", reqs[0].body)
			}
			if !tc.wantTainted {
				if _, present := reqs[0].body["link_preview_options"]; present {
					t.Errorf("clean telegram delivery disabled previews: %v", reqs[0].body)
				}
			}
		})

		t.Run("discord/"+tc.name, func(t *testing.T) {
			dcTransport.reset()
			req := &turn.TurnRequest{
				SessionID:      "main/discord:home/dm:dc-" + strconv.Itoa(i),
				PrincipalID:    principal.OwnerID,
				PrincipalClass: principal.ClassOwner,
				Origin:         turn.Origin("discord:home"),
				Parts:          []runtimeingress.InputPart{{Text: "hello"}},
				ReplyContext:   dcReply,
				IdempotencyKey: "dc-preview-" + strconv.Itoa(i),
			}
			if tc.mutate != nil {
				tc.mutate(req)
			}
			runTurn(t, req)
			reqs := dcTransport.messages("/messages")
			if len(reqs) != 1 {
				t.Fatalf("discord sends = %d, want 1", len(reqs))
			}
			suppressed := discordEmbedsSuppressed(reqs[0].body)
			if tc.wantTainted && !suppressed {
				t.Errorf("tainted discord delivery kept embeds: %v", reqs[0].body)
			}
			if !tc.wantTainted {
				if _, present := reqs[0].body["flags"]; present {
					t.Errorf("clean discord delivery suppressed embeds: %v", reqs[0].body)
				}
			}
		})
	}
}

func newBroadcastPreviewRegistry(t *testing.T, withKnowledge bool) *auraagent.Registry {
	t.Helper()
	cfg := appconfig.Default()
	persona := auraagent.Persona{
		ID:           auraagent.DefaultID,
		Name:         "Main",
		Description:  "default",
		Instructions: "answer",
		Tools:        []string{"web_search", "web_fetch"},
	}
	if withKnowledge {
		persona.Knowledge = []string{"notes"}
	}
	cfg.Agents.Definitions = []auraagent.Persona{persona}
	registry, err := buildAgentRegistry(&cfg)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	return registry
}

func driveBroadcastItem(t *testing.T, store *broadcast.SQLiteStore, runner *broadcast.Runner, id string) {
	t.Helper()
	clock := durabletest.NewManualClock(time.Now().UTC())
	rt := durabletest.NewFake().WithClock(clock)
	rt.RegisterHandler(broadcast.HandlerName, runner.Handle)
	t.Cleanup(rt.Close)
	ref, err := rt.Start(t.Context(), durable.StartRequest{
		Handler: broadcast.HandlerName, Key: id, Payload: []byte(id),
	})
	if err != nil {
		t.Fatalf("Start broadcast %s: %v", id, err)
	}
	rt.WaitReady(ref)
	item, found, err := store.Load(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("Load %s: found=%t err=%v", id, found, err)
	}
	if item.State != broadcast.StateSucceeded && item.State != broadcast.StateCancelled {
		t.Fatalf("item %s state = %q, want terminal delivered or digested", id, item.State)
	}
}

func TestTaintedBroadcastNeverShowsLinkPreviews(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schemas()...)
	sessions := turnstore.NewSessionService(db)
	effects, err := newChannelEffects(db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newChannelEffects: %v", err)
	}
	dcTransport := &recordingTransport{}
	dcAdapter := newOptedInDiscordAdapter(t, db, effects, dcTransport)
	sender := &discordBroadcastSender{adapter: dcAdapter, effects: effects, sessions: sessions}

	store, err := broadcast.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("broadcast.NewSQLiteStore: %v", err)
	}
	routes := map[string]string{"default": "discord:333"}
	runner, err := broadcast.NewRunner(
		store,
		routes,
		map[string]broadcast.Sender{"discord": sender},
		broadcast.RunPolicy{
			MaxAttempts: 3, MaxDeliveryAge: 24 * time.Hour, DispatchGap: 0,
			Fallback: map[string]string{}, MaxDigestItems: 20, MaxDigestBytes: 12000,
		},
		slog.New(slog.DiscardHandler),
		nil,
	)
	if err != nil {
		t.Fatalf("broadcast.NewRunner: %v", err)
	}
	broadcaster, err := broadcast.NewBroadcaster(
		broadcast.Policy{Destinations: routes, MaxDigestItems: 20, MaxDigestBytes: 12000},
		testChannelRegistry{"discord": true},
		store,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("broadcast.NewBroadcaster: %v", err)
	}

	t.Run("direct tainted broadcast disables previews while clean keeps previews", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			key     string
			tainted bool
		}{
			{name: "clean", key: "direct-clean", tainted: false},
			{name: "tainted", key: "direct-tainted", tainted: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dcTransport.reset()
				item, _, err := broadcaster.Submit(t.Context(), &broadcast.Notification{
					Producer:         "cron",
					IdempotencyKey:   tc.key,
					Priority:         broadcast.PriorityWarning,
					DestinationAlias: "default",
					ContentJSON:      `{"text":"see https://evil.example/?d=` + tc.key + `"}`,
					CreatedAt:        time.Now().UTC(),
					Tainted:          tc.tainted,
				})
				if err != nil {
					t.Fatalf("Submit: %v", err)
				}
				loaded, err := store.Item(t.Context(), item.ID)
				if err != nil || loaded.Tainted != tc.tainted {
					t.Fatalf("stored item tainted = %t (err %v), want %t", loaded.Tainted, err, tc.tainted)
				}
				driveBroadcastItem(t, store, runner, item.ID)
				reqs := dcTransport.messages("/messages")
				if len(reqs) != 1 {
					t.Fatalf("discord sends = %d, want 1", len(reqs))
				}
				if tc.tainted {
					if !discordEmbedsSuppressed(reqs[0].body) {
						t.Errorf("tainted broadcast kept embeds: %v", reqs[0].body)
					}
				} else if _, present := reqs[0].body["flags"]; present {
					t.Errorf("clean broadcast suppressed embeds: %v", reqs[0].body)
				}
			})
		}
	})

	t.Run("digest containing a tainted item disables previews while clean digest keeps previews", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			secondTaint bool
			windowShift time.Duration
		}{
			{name: "clean digest", secondTaint: false, windowShift: -2 * time.Hour},
			{name: "mixed tainted digest", secondTaint: true, windowShift: -time.Hour},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dcTransport.reset()
				release := time.Now().UTC().Truncate(time.Second).Add(tc.windowShift)
				firstID := "bcst-dg-a-" + strings.ReplaceAll(tc.name, " ", "-")
				secondID := "bcst-dg-b-" + strings.ReplaceAll(tc.name, " ", "-")
				for _, rec := range []*broadcast.ItemRecord{
					{
						ID: firstID, Producer: "cron", IdempotencyKey: "k-" + firstID, ContentDigest: "d-" + firstID,
						Priority: broadcast.PriorityInfo, DestinationAlias: "default", ContentJSON: `{"text":"https://clean.example/a"}`,
						State: broadcast.StateHeld, NotBefore: release, CreatedAt: release.Add(-10 * time.Minute), UpdatedAt: release.Add(-10 * time.Minute),
						Tainted: false,
					},
					{
						ID: secondID, Producer: "cron", IdempotencyKey: "k-" + secondID, ContentDigest: "d-" + secondID,
						Priority: broadcast.PriorityInfo, DestinationAlias: "default", ContentJSON: `{"text":"https://evil.example/b"}`,
						State: broadcast.StateHeld, NotBefore: release, CreatedAt: release.Add(-5 * time.Minute), UpdatedAt: release.Add(-5 * time.Minute),
						Tainted: tc.secondTaint,
					},
				} {
					if _, _, err := store.Insert(t.Context(), rec); err != nil {
						t.Fatalf("Insert %s: %v", rec.ID, err)
					}
				}
				driveBroadcastItem(t, store, runner, firstID)
				child, err := store.Item(t.Context(), firstID)
				if err != nil || child.DigestParentID == "" {
					t.Fatalf("digest child = %+v, %v", child, err)
				}
				parent, err := store.Item(t.Context(), child.DigestParentID)
				if err != nil || parent.Tainted != tc.secondTaint {
					t.Fatalf("digest parent tainted = %t (err %v), want %t", parent.Tainted, err, tc.secondTaint)
				}
				reqs := dcTransport.messages("/messages")
				if len(reqs) != 1 {
					t.Fatalf("digest discord sends = %d, want 1", len(reqs))
				}
				if tc.secondTaint {
					if !discordEmbedsSuppressed(reqs[0].body) {
						t.Errorf("tainted digest kept embeds: %v", reqs[0].body)
					}
				} else if _, present := reqs[0].body["flags"]; present {
					t.Errorf("clean digest suppressed embeds: %v", reqs[0].body)
				}
			})
		}
	})

	t.Run("scheduled notifications disable previews for tainted web-only and knowledge-scoped runs", func(t *testing.T) {
		notifier := &cronNotifier{broadcaster: broadcaster}
		jobStore, err := schedule.NewSQLiteStore(db)
		if err != nil {
			t.Fatalf("schedule.NewSQLiteStore: %v", err)
		}
		jobSvc, err := schedule.NewService(jobStore)
		if err != nil {
			t.Fatalf("schedule.NewService: %v", err)
		}
		cases := []struct {
			name          string
			jobContext    string
			jobTaint      string
			withKnowledge bool
			wantTainted   bool
		}{
			{name: "clean full job on clean persona", jobContext: schedule.ContextFull, wantTainted: false},
			{name: "tainted full job", jobContext: schedule.ContextFull, jobTaint: "a webhook", wantTainted: true},
			{name: "clean web-only job", jobContext: schedule.ContextWebOnly, wantTainted: true},
			{name: "full job on knowledge persona", jobContext: schedule.ContextFull, withKnowledge: true, wantTainted: true},
		}
		for i, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				dcTransport.reset()
				var createdIDs []string
				itemBroadcaster, err := broadcast.NewBroadcaster(
					broadcast.Policy{Destinations: routes, MaxDigestItems: 20, MaxDigestBytes: 12000},
					testChannelRegistry{"discord": true},
					store,
					func(_ context.Context, itemID string) error {
						createdIDs = append(createdIDs, itemID)
						return nil
					},
					nil,
				)
				if err != nil {
					t.Fatalf("NewBroadcaster: %v", err)
				}
				notifier.broadcaster = itemBroadcaster
				fakeEngine := &fakeCronEngine{
					batches: [][]turn.Event{cronCompletedBatch("turn-cron-"+strconv.Itoa(i), "https://evil.example/cron-"+strconv.Itoa(i))},
				}
				turnRunner := &engineTurnRunner{
					engine:   fakeEngine,
					sessions: sessions,
					personas: newBroadcastPreviewRegistry(t, tc.withKnowledge),
				}
				schedRuntime := durabletest.NewFake()
				t.Cleanup(schedRuntime.Close)
				schedRunner, err := schedule.NewRunner(&schedule.RunnerDeps{
					Jobs:    jobStore,
					Turns:   turnRunner,
					Notify:  notifier,
					Starter: schedRuntime,
					Logger:  slog.New(slog.DiscardHandler),
				})
				if err != nil {
					t.Fatalf("schedule.NewRunner: %v", err)
				}
				schedRuntime.RegisterHandler(schedule.HandlerName, schedRunner.Handle)
				job, err := jobSvc.Create(t.Context(), &schedule.JobSpec{
					Name: "cron-" + strconv.Itoa(i), CronExpression: "* * * * *", Timezone: "UTC",
					Prompt: "check https://example.com", Context: tc.jobContext, TaintSource: tc.jobTaint,
					OriginChannel: "discord", OriginDestination: "default",
					OverlapPolicy: schedule.OverlapSkip, CatchUpGraceSeconds: 900,
				})
				if err != nil {
					t.Fatalf("Create job: %v", err)
				}
				fire := time.Now().UTC().Truncate(time.Second).Add(time.Duration(i+1) * time.Nanosecond)
				occ := &schedule.Occurrence{
					ID: "occ-preview-" + strconv.Itoa(i), JobID: job.ID, JobVersion: job.Version,
					ScheduledForUTC: fire, State: schedule.OccurrenceFired, CreatedAt: fire, UpdatedAt: fire,
				}
				if err := jobStore.RecordFire(t.Context(), occ); err != nil {
					t.Fatalf("RecordFire: %v", err)
				}
				payload, err := json.Marshal(map[string]any{"job_id": job.ID, "instant": fire.Format(time.RFC3339Nano), "chain": false})
				if err != nil {
					t.Fatalf("marshal fire: %v", err)
				}
				ref, err := schedRuntime.Start(t.Context(), durable.StartRequest{
					Handler: schedule.HandlerName, Key: "sched-preview-" + strconv.Itoa(i), Payload: payload,
				})
				if err != nil {
					t.Fatalf("Start schedule: %v", err)
				}
				schedRuntime.WaitReady(ref)
				if len(createdIDs) != 1 {
					t.Fatalf("broadcast items created = %v, want 1", createdIDs)
				}
				item, err := store.Item(t.Context(), createdIDs[0])
				if err != nil || item.Tainted != tc.wantTainted {
					t.Fatalf("scheduled broadcast item tainted = %t (err %v), want %t", item.Tainted, err, tc.wantTainted)
				}
				driveBroadcastItem(t, store, runner, item.ID)
				reqs := dcTransport.messages("/messages")
				if len(reqs) != 1 {
					t.Fatalf("discord sends = %d, want 1", len(reqs))
				}
				if tc.wantTainted {
					if !discordEmbedsSuppressed(reqs[0].body) {
						t.Errorf("scheduled %s kept embeds: %v", tc.name, reqs[0].body)
					}
				} else if _, present := reqs[0].body["flags"]; present {
					t.Errorf("scheduled %s suppressed embeds: %v", tc.name, reqs[0].body)
				}
			})
		}
	})
}

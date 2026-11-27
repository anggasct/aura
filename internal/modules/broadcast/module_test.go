package broadcast

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/durable/durabletest"
	durablesqlite "github.com/anggasct/aura/internal/platform/durable/sqlite"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

type stubSender struct{}

func (stubSender) Send(context.Context, *SendRequest) (SendOutcome, error) {
	return SendOutcome{Succeeded: true}, nil
}

func TestBroadcastModuleNewValidatesDepsAndConfig(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), durablesqlite.Schema(), effect.Schema(), Schema())
	rt := durabletest.NewFake()
	validCfg := Default()

	badTZ := Default()
	badTZ.Timezone = "   "
	invalidTZ := Default()
	invalidTZ.Timezone = "Mars/Olympus"
	badStart := Default()
	badStart.QuietHours.Start = "25:00"
	badEnd := Default()
	badEnd.QuietHours.End = "nope"

	cases := []struct {
		name string
		deps *ModuleDeps
	}{
		{"nil deps", nil},
		{"nil db", &ModuleDeps{Config: validCfg, Runtime: rt}},
		{"nil runtime", &ModuleDeps{Config: validCfg, DB: db}},
		{"empty timezone", &ModuleDeps{Config: badTZ, DB: db, Runtime: rt}},
		{"invalid timezone", &ModuleDeps{Config: invalidTZ, DB: db, Runtime: rt}},
		{"invalid quiet start", &ModuleDeps{Config: badStart, DB: db, Runtime: rt}},
		{"invalid quiet end", &ModuleDeps{Config: badEnd, DB: db, Runtime: rt}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.deps); !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("New() err = %v, want invalid_argument", err)
			}
		})
	}
}

func TestBroadcastModuleClonesSendersAndExcludesNilSender(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), durablesqlite.Schema(), effect.Schema(), Schema())
	rt := durabletest.NewFake()
	cfg := Default()
	cfg.Destinations = map[string]string{
		"live": "discord:owner",
		"dead": "telegram:owner",
	}
	senders := map[string]Sender{
		"discord":  stubSender{},
		"telegram": nil,
	}
	mod, err := New(&ModuleDeps{
		Config:  cfg,
		DB:      db,
		Senders: senders,
		Runtime: rt,
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	delete(senders, "discord")
	senders["telegram"] = stubSender{}

	if _, err := mod.Broadcaster().Resolve("live"); err != nil {
		t.Fatalf("Resolve(live) after caller mutation: %v", err)
	}
	if _, err := mod.Broadcaster().Resolve("dead"); !fault.HasCode(err, ErrorCodeChannelUnknown) {
		t.Fatalf("Resolve(dead) = %v, want channel_unknown", err)
	}
}

func TestBroadcastModuleSubmitAndResume(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, turnstore.Schema(), durablesqlite.Schema(), effect.Schema(), Schema())
	rt := durabletest.NewFake()
	rt.RegisterHandler(HandlerName, func(context.Context, durable.Invocation) error { return nil })

	cfg := Default()
	cfg.QuietHours.Enabled = false
	cfg.Destinations = map[string]string{"default": "discord:owner"}
	mod, err := New(&ModuleDeps{
		Config:  cfg,
		DB:      db,
		Senders: map[string]Sender{"discord": stubSender{}},
		Runtime: rt,
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if mod.Store() == nil || mod.Runner() == nil || mod.Broadcaster() == nil {
		t.Fatal("module getters returned nil")
	}

	item, replayed, err := mod.Broadcaster().Submit(t.Context(), &Notification{
		Producer:         "cron",
		IdempotencyKey:   "k1",
		Priority:         PriorityInfo,
		DestinationAlias: "default",
		ContentJSON:      `{"text":"hi"}`,
		CreatedAt:        time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC),
	})
	if err != nil || replayed {
		t.Fatalf("Submit(): item=%+v replayed=%v err=%v", item, replayed, err)
	}
	if _, err := rt.Status(t.Context(), durable.RunRef{Key: item.ID}); err != nil {
		t.Fatalf("durable run for submitted item missing: %v", err)
	}

	resumed, err := mod.Resume(t.Context())
	if err != nil {
		t.Fatalf("Resume(): %v", err)
	}
	if resumed != 1 {
		t.Fatalf("Resume() = %d, want 1", resumed)
	}
}

package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	kitapproval "github.com/anggasct/aura/internal/channels/kit/approval"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/store"
)

type telegramOriginAccess struct{}

func (telegramOriginAccess) ToolCeiling(agentID string, class principal.Class) ([]string, bool) {
	if agentID != auraagent.DefaultID || class != principal.ClassTrusted {
		return nil, false
	}
	return []string{"exec"}, true
}

func (telegramOriginAccess) StandingGrant(context.Context, string, string, string, string) (bool, error) {
	return false, nil
}

func TestTelegramTrustedEffectfulToolFailsClosedWithoutAnApprovalTarget(t *testing.T) {
	both := enabledDiscordConfig()
	telegramCfg := enabledTelegramConfig()
	both.Owner = telegramCfg.Owner
	both.Channels.Telegram = telegramCfg.Channels.Telegram
	cases := map[string]struct {
		cfg        appconfig.Config
		wantDecide bool
	}{
		"telegram only":        {cfg: telegramCfg, wantDecide: true},
		"discord and telegram": {cfg: both, wantDecide: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := discordTestDB(t)
			cfg := tc.cfg
			_, _, approvers, err := buildChannelAdapters(&cfg, testRouter(t, &cfg), db, t.TempDir(), nil, nil, nil)
			if err != nil {
				t.Fatalf("buildChannelAdapters(): %v", err)
			}
			decider := kitapproval.Decider(approvalChannels(approvers, nil))
			if (decider != nil) != tc.wantDecide {
				t.Fatalf("decider present = %v, want %v", decider != nil, tc.wantDecide)
			}
			const sessionID = "main/telegram:pocket/chat:5"
			now := time.Now().UTC()
			if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{ID: sessionID, OwnerID: "telegram:5", AgentID: auraagent.DefaultID, CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
				t.Fatalf("Create session: %v", err)
			}
			executor, err := newTestToolExecutor(&toolExecutorInput{
				result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: capabilitytest.All(t)},
				db:           db,
				artifactRoot: t.TempDir(),
				logger:       slog.New(slog.DiscardHandler),
				wiring:       &toolWiring{decider: decider, access: telegramOriginAccess{}},
				events:       turn.NewEventBus(slog.New(slog.DiscardHandler)),
			})
			if err != nil {
				t.Fatalf("newTestToolExecutor: %v", err)
			}
			_, err = executor.Broker().Execute(t.Context(), &toolbroker.ToolRequest{
				RequestID:      "request-1",
				TurnID:         "turn-1",
				SessionID:      sessionID,
				PrincipalID:    "telegram:5",
				PrincipalClass: principal.ClassTrusted,
				AgentID:        auraagent.DefaultID,
				ToolName:       "exec",
				ToolVersion:    "v1",
				Arguments:      json.RawMessage(`{"command":["true"]}`),
				Capabilities:   []string{"exec-linux"},
				Trust:          approval.TrustOwnerInput,
				IdempotencyKey: "idempotency-1",
				EventSequence:  1,
			})
			if !fault.HasCode(err, toolbroker.ResultApprovalRequired) {
				t.Fatalf("Execute error = %v, want approval_required (fail closed)", err)
			}
		})
	}
}

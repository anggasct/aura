package cli

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/toolbroker"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
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
		cfg        config.Config
		wantDecide bool
	}{
		"telegram only":        {cfg: telegramCfg, wantDecide: true},
		"discord and telegram": {cfg: both, wantDecide: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			db := discordTestDB(t)
			cfg := tc.cfg
			_, _, approvers, err := buildChannelAdapters(&cfg, testRouter(t, &cfg), db, t.TempDir(), nil, nil)
			if err != nil {
				t.Fatalf("buildChannelAdapters(): %v", err)
			}
			decider := channelApprovalDecider(approvalChannels(approvers, nil))
			if (decider != nil) != tc.wantDecide {
				t.Fatalf("decider present = %v, want %v", decider != nil, tc.wantDecide)
			}
			const sessionID = "main/telegram:pocket/chat:5"
			now := time.Now().UTC()
			if err := store.NewSessionService(db).Create(t.Context(), &store.Session{ID: sessionID, OwnerID: "telegram:5", AgentID: auraagent.DefaultID, CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
				t.Fatalf("Create session: %v", err)
			}
			executor, err := toolsbuiltin.New(&cfg, db, t.TempDir(), slog.New(slog.DiscardHandler), nil, decider, telegramOriginAccess{})
			if err != nil {
				t.Fatalf("toolsbuiltin.New: %v", err)
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
			if class, ok := toolbroker.CodeOf(err); !ok || class != toolbroker.ResultApprovalRequired {
				t.Fatalf("Execute error = %v, want approval_required (fail closed)", err)
			}
		})
	}
}

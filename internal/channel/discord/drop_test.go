package discord

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type dropRecorder struct {
	mu      sync.Mutex
	reasons []string
}

func (r *dropRecorder) observe(_ context.Context, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = append(r.reasons, reason)
}

func dropChannelStub(t *testing.T) *restStub {
	t.Helper()
	channels := map[string]threadChannelResponse{
		"333": {ID: "333"},
		"334": {ID: "334"},
		"336": {ID: "336", Type: channelTypePublicThread, ParentID: "333", OwnerID: "888"},
		"337": {ID: "337"},
	}
	return newRestStub(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"5000"}`))
			return
		}
		info, ok := channels[strings.TrimPrefix(r.URL.Path, "/channels/")]
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	})
}

func dropRecords(logs *logCapture) []string {
	var records []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `msg="discord message dropped"`) {
			records = append(records, line)
		}
	}
	return records
}

func TestAdmitDropsSayWhy(t *testing.T) {
	threadType := channelTypePublicThread
	cases := map[string]struct {
		persona string
		pairing bool
		setup   func(a *Adapter)
		msg     func() *messagePayload
		reason  dropReason
		scope   string
		level   string
	}{
		"bot author": {
			msg: func() *messagePayload {
				msg := textMessage("9401", "4400", "", "555", "SECRET-CONTENT", nil)
				msg.Author.Bot = true
				return msg
			},
			reason: dropAuthorBot, scope: dropScopeDM, level: "DEBUG",
		},
		"system message type": {
			msg: func() *messagePayload {
				msg := textMessage("9402", "4400", "", "777", "SECRET-CONTENT", nil)
				msg.Type = 7
				return msg
			},
			reason: dropMessageType, scope: dropScopeDM, level: "INFO",
		},
		"empty content": {
			msg:    func() *messagePayload { return textMessage("9403", "4400", "", "777", "", nil) },
			reason: dropEmptyContent, scope: dropScopeDM, level: "INFO",
		},
		"stranger dm": {
			msg:    func() *messagePayload { return textMessage("9404", "4400", "", "666", "SECRET-CONTENT", nil) },
			reason: dropAuthorNotAllowed, scope: dropScopeDM, level: "DEBUG",
		},
		"guild outside scope": {
			msg: func() *messagePayload {
				return textMessage("9405", "333", "223", "777", "SECRET-CONTENT", []string{"999"})
			},
			reason: dropGuildNotAllowed, scope: dropScopeChannel, level: "INFO",
		},
		"channel outside scope": {
			msg: func() *messagePayload {
				return textMessage("9406", "334", "222", "777", "SECRET-CONTENT", []string{"999"})
			},
			reason: dropChannelNotAllowed, scope: dropScopeChannel, level: "INFO",
		},
		"owner without mention": {
			msg:    func() *messagePayload { return textMessage("9407", "333", "222", "777", "SECRET-CONTENT", nil) },
			reason: dropNotMentioned, scope: dropScopeChannel, level: "INFO",
		},
		"dms closed": {
			setup:  func(a *Adapter) { a.account.AcceptDMs = false },
			msg:    func() *messagePayload { return textMessage("9408", "4400", "", "777", "SECRET-CONTENT", nil) },
			reason: dropDMsDisabled, scope: dropScopeDM, level: "INFO",
		},
		"pairing reply": {
			persona: "office", pairing: true,
			msg:    func() *messagePayload { return textMessage("9409", "4400", "", "666", "SECRET-CONTENT", nil) },
			reason: dropPairing, scope: dropScopeDM, level: "DEBUG",
		},
		"thread probe failure": {
			msg:    func() *messagePayload { return textMessage("9410", "335", "222", "777", "SECRET-CONTENT", nil) },
			reason: dropThreadProbeFailed, scope: dropScopeThread, level: "INFO",
		},
		"foreign thread": {
			msg:    func() *messagePayload { return textMessage("9411", "336", "222", "777", "SECRET-CONTENT", nil) },
			reason: dropThreadForeign, scope: dropScopeThread, level: "INFO",
		},
		"unclassified thread": {
			msg: func() *messagePayload {
				msg := textMessage("9412", "337", "222", "777", "SECRET-CONTENT", nil)
				msg.ChannelType = &threadType
				return msg
			},
			reason: dropThreadUnresolved, scope: dropScopeThread, level: "INFO",
		},
		"no persona route": {
			setup:  func(a *Adapter) { a.router = nil },
			msg:    func() *messagePayload { return textMessage("9413", "4400", "", "777", "SECRET-CONTENT", nil) },
			reason: dropPersonaUnrouted, scope: dropScopeDM, level: "INFO",
		},
		"audience refuses trusted": {
			msg:    func() *messagePayload { return textMessage("9414", "4400", "", "111", "SECRET-CONTENT", nil) },
			reason: dropAudience, scope: dropScopeDM, level: "INFO",
		},
		"attachment intake failure": {
			msg: func() *messagePayload {
				msg := textMessage("9415", "4400", "", "777", "SECRET-CONTENT", nil)
				msg.Attachments = make([]attachmentPayload, maxAttachmentsPerMessage+1)
				return msg
			},
			reason: dropAttachmentFailed, scope: dropScopeDM, level: "INFO",
		},
		"stranger in a bot thread": {
			setup:  func(a *Adapter) { a.rememberThread("9300", "7770", threadRoute{parent: "333", persona: "owneronly"}) },
			msg:    func() *messagePayload { return textMessage("9416", "7770", "222", "666", "SECRET-CONTENT", nil) },
			reason: dropAuthorNotAllowed, scope: dropScopeThread, level: "DEBUG",
		},
		"bot thread outside channel scope": {
			setup:  func(a *Adapter) { a.rememberThread("9301", "7771", threadRoute{parent: "339", persona: "owneronly"}) },
			msg:    func() *messagePayload { return textMessage("9417", "7771", "222", "777", "SECRET-CONTENT", nil) },
			reason: dropChannelNotAllowed, scope: dropScopeThread, level: "INFO",
		},
		"bot thread without a persona": {
			setup: func(a *Adapter) {
				a.rememberThread("9302", "7772", threadRoute{parent: "333"})
				a.router = nil
			},
			msg:    func() *messagePayload { return textMessage("9418", "7772", "222", "777", "SECRET-CONTENT", nil) },
			reason: dropPersonaUnrouted, scope: dropScopeThread, level: "INFO",
		},
		"trusted in an owner-only bot thread": {
			setup:  func(a *Adapter) { a.rememberThread("9303", "7773", threadRoute{parent: "333", persona: "owneronly"}) },
			msg:    func() *messagePayload { return textMessage("9419", "7773", "222", "111", "SECRET-CONTENT", nil) },
			reason: dropAudience, scope: dropScopeThread, level: "INFO",
		},
		"unevaluated persona": {
			persona: "team",
			setup:   func(a *Adapter) { a.access.Evals = &fakeEvalGate{evaluated: map[string]bool{}} },
			msg:     func() *messagePayload { return textMessage("9420", "4400", "", "111", "SECRET-CONTENT", nil) },
			reason:  dropPersonaEvalMissing, scope: dropScopeDM, level: "INFO",
		},
		"unevaluated persona in a bot thread": {
			setup: func(a *Adapter) {
				a.access.Evals = &fakeEvalGate{evaluated: map[string]bool{}}
				a.rememberThread("9305", "7775", threadRoute{parent: "333", persona: "team"})
			},
			msg:    func() *messagePayload { return textMessage("9421", "7775", "222", "111", "SECRET-CONTENT", nil) },
			reason: dropPersonaEvalMissing, scope: dropScopeThread, level: "INFO",
		},
		"rate limited": {
			persona: "team",
			setup:   func(a *Adapter) { a.access.Budgets = &fakeBudgetGate{refusal: BudgetRefusalRateLimited} },
			msg:     func() *messagePayload { return textMessage("9422", "4400", "", "111", "SECRET-CONTENT", nil) },
			reason:  dropRateLimited, scope: dropScopeDM, level: "INFO",
		},
		"budget exhausted in a bot thread": {
			setup: func(a *Adapter) {
				a.access.Budgets = &fakeBudgetGate{refusal: BudgetRefusalExhausted}
				a.rememberThread("9306", "7776", threadRoute{parent: "333", persona: "team"})
			},
			msg:    func() *messagePayload { return textMessage("9423", "7776", "222", "111", "SECRET-CONTENT", nil) },
			reason: dropBudgetExhausted, scope: dropScopeThread, level: "INFO",
		},
		"budget check failure": {
			persona: "team",
			setup:   func(a *Adapter) { a.access.Budgets = &fakeBudgetGate{err: errors.New("ledger unavailable")} },
			msg:     func() *messagePayload { return textMessage("9424", "4400", "", "111", "SECRET-CONTENT", nil) },
			reason:  dropBudgetCheckFailed, scope: dropScopeDM, level: "INFO",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			persona := tc.persona
			if persona == "" {
				persona = "owneronly"
			}
			pairing := &fakePairing{outcome: PairingOutcomeIssued}
			adapter, sink, _ := newPairingAdapter(t, persona, pairing)
			if !tc.pairing {
				adapter.access.Pairing = nil
			}
			adapter.restBase = dropChannelStub(t).srv.URL
			logs := &logCapture{}
			adapter.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			drops := &dropRecorder{}
			adapter.SetDropObserver(drops.observe)
			if tc.setup != nil {
				tc.setup(adapter)
			}
			msg := tc.msg()
			admitted, err := adapter.admit(t.Context(), msg)
			if err != nil || admitted {
				t.Fatalf("admit() = %v, %v; want dropped without error", admitted, err)
			}
			if sink.calls["message:"+msg.ID] != 0 {
				t.Fatal("dropped message reached the ingress sink")
			}
			records := dropRecords(logs)
			if len(records) != 1 {
				t.Fatalf("drop records = %d, want 1:\n%s", len(records), logs.String())
			}
			record := records[0]
			for _, want := range []string{"level=" + tc.level, "reason=" + string(tc.reason), "scope=" + tc.scope, "account=owner", "component=discord"} {
				if !strings.Contains(record, want) {
					t.Errorf("drop record %q lacks %q", record, want)
				}
			}
			for _, leak := range []string{"SECRET-CONTENT", msg.ID, msg.ChannelID, msg.Author.ID, "222", "223"} {
				if strings.Contains(record, leak) {
					t.Errorf("drop record %q leaks %q", record, leak)
				}
			}
			if len(drops.reasons) != 1 || drops.reasons[0] != string(tc.reason) {
				t.Errorf("observed drops = %v, want [%s]", drops.reasons, tc.reason)
			}
		})
	}
}

func TestAdmittedMessagesLeaveNoDropRecord(t *testing.T) {
	adapter, sink, _ := newPairingAdapter(t, "owneronly", &fakePairing{})
	adapter.restBase = dropChannelStub(t).srv.URL
	logs := &logCapture{}
	adapter.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	drops := &dropRecorder{}
	adapter.SetDropObserver(drops.observe)
	adapter.rememberThread("9304", "7774", threadRoute{parent: "333", persona: "owneronly"})
	for _, msg := range []*messagePayload{
		textMessage("9501", "4400", "", "777", "hello", nil),
		textMessage("9502", "7774", "222", "777", "follow up", nil),
	} {
		if admitted, err := adapter.admit(t.Context(), msg); err != nil || !admitted {
			t.Fatalf("admit(%s) = %v, %v; want admitted", msg.ID, admitted, err)
		}
		routedEnvelope(t, sink, "message:"+msg.ID)
	}
	if records := dropRecords(logs); len(records) != 0 || len(drops.reasons) != 0 {
		t.Fatalf("admitted messages logged drops %v / %v", records, drops.reasons)
	}
}

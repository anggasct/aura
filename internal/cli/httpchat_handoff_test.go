package cli

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/channels/httpchat"
	toolsbuiltin "github.com/anggasct/aura/internal/modules/builtin"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

const (
	handoffTurnReason    = "customer asks for a refund"
	handoffTurnHandedOff = "handed over to the owner"
	handoffTurnFailed    = "the owner could not be notified"
)

type handoffTurnRuntime struct {
	opener toolsbuiltin.HandoffOpener
	turns  int
}

func (r *handoffTurnRuntime) Run(ctx context.Context, req *turn.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	r.turns++
	return func(yield func(store.RuntimeEvent, error) bool) {
		if !yield(store.RuntimeEvent{TurnID: "turn-1", Kind: turn.EventKindTurnAccepted}, nil) {
			return
		}
		text := handoffTurnHandedOff
		if err := r.opener.OpenHandoff(ctx, &toolsbuiltin.HandoffRequest{
			SessionID: req.SessionID, PrincipalID: req.PrincipalID, AgentID: req.AgentID, Reason: handoffTurnReason,
		}); err != nil {
			text = handoffTurnFailed
		}
		answer := store.RuntimeEvent{TurnID: "turn-1", Kind: turn.EventKindMessageCompleted}
		answer.Payload, _ = json.Marshal(map[string]string{"text": text})
		if !yield(answer, nil) {
			return
		}
		yield(store.RuntimeEvent{TurnID: "turn-1", Kind: turn.EventKindTurnCompleted}, nil)
	}
}

type handoffHTTPHarness struct {
	*httpChatHarness
	runtime *handoffTurnRuntime
	log     *askLog
}

func newHandoffHTTPHarness(t *testing.T, build func(log *askLog) []approvalAdapter) *handoffHTTPHarness {
	t.Helper()
	path := writeHTTPChatRuntimeConfig(t, "50")
	cfg := mustLoadConfig(t, path)
	db := openAccessDB(t, path)
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	passEvals(t, db, registry, "lobby", "staff")
	log := &askLog{}
	adapters := build(log)
	approvals, err := newOwnerRoute(adapters, (&routedResults{}).observe)
	if err != nil {
		t.Fatalf("newOwnerRoute: %v", err)
	}
	service := newHandoffService(db, []handoffNotifier{newHTTPHandoffNotifier(approvals, adapters)})
	backend := &handoffTurnRuntime{opener: service}
	server, _, err := buildHTTPChat(cfg, db, backend, registry, nil, approvals, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildHTTPChat: %v", err)
	}
	harness := &httpChatHarness{t: t, config: path, baseURL: serveHTTPChat(t, server), db: db}
	return &handoffHTTPHarness{httpChatHarness: harness, runtime: backend, log: log}
}

type repliesBody struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Content string `json:"content"`
	} `json:"data"`
	HandoffOpen bool `json:"handoff_open"`
}

func (h *httpChatHarness) replies(key, session, query string) (int, repliesBody) {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet, h.baseURL+"/v1/handoff/replies"+query, http.NoBody)
	if err != nil {
		h.t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if session != "" {
		req.Header.Set(httpchat.SessionHeader, session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	var decoded repliesBody
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			h.t.Fatalf("decode replies %q: %v", raw, err)
		}
	}
	return resp.StatusCode, decoded
}

func TestHTTPTurnHandoffNotifiesTheOwnerAndHoldsTheSession(t *testing.T) {
	cases := []struct {
		name       string
		class      string
		build      func(log *askLog) []approvalAdapter
		wantAnswer string
		wantAsked  []string
		wantOpened bool
	}{
		{
			name:  "public key reaches the persona serving adapter",
			class: "public",
			build: func(log *askLog) []approvalAdapter {
				return []approvalAdapter{
					&scriptedApprover{name: "discord", log: log},
					&scriptedApprover{name: "telegram", serves: "lobby", log: log},
				}
			},
			wantAnswer: handoffTurnHandedOff, wantAsked: []string{"telegram"}, wantOpened: true,
		},
		{
			name:  "trusted key falls through an unreachable adapter",
			class: "trusted",
			build: func(log *askLog) []approvalAdapter {
				return []approvalAdapter{
					&scriptedApprover{name: "discord", serves: "lobby", handoffErr: discordUnavailable(), log: log},
					&scriptedApprover{name: "telegram", log: log},
				}
			},
			wantAnswer: handoffTurnHandedOff, wantAsked: []string{"discord", "telegram"}, wantOpened: true,
		},
		{
			name:  "telegram without an owner identity falls through to discord",
			class: "public",
			build: func(log *askLog) []approvalAdapter {
				return []approvalAdapter{
					&scriptedApprover{name: "discord", log: log},
					&scriptedApprover{name: "telegram", serves: "lobby", handoffErr: telegramHandoffUnavailable(), log: log},
				}
			},
			wantAnswer: handoffTurnHandedOff, wantAsked: []string{"telegram", "discord"}, wantOpened: true,
		},
		{
			name:  "no reachable adapter fails the tool and opens nothing",
			class: "public",
			build: func(log *askLog) []approvalAdapter {
				return []approvalAdapter{
					&scriptedApprover{name: "discord", handoffErr: discordUnavailable(), log: log},
					&scriptedApprover{name: "telegram", handoffErr: telegramHandoffUnavailable(), log: log},
				}
			},
			wantAnswer: handoffTurnFailed, wantAsked: []string{"discord", "telegram"},
		},
		{
			name:       "no adapters at all fails the tool and opens nothing",
			class:      "public",
			build:      func(*askLog) []approvalAdapter { return nil },
			wantAnswer: handoffTurnFailed,
			wantAsked:  []string{},
		},
		{
			name:  "owner key keeps the failure",
			class: "owner",
			build: func(log *askLog) []approvalAdapter {
				return []approvalAdapter{&scriptedApprover{name: "telegram", serves: "lobby", log: log}}
			},
			wantAnswer: handoffTurnFailed, wantAsked: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandoffHTTPHarness(t, tc.build)
			var key, keyID string
			if tc.class == "owner" {
				key, keyID = h.ownerKey()
			} else {
				key, keyID = h.key("lobby", tc.class)
			}
			first := h.post(key, chatJSON(chatMessage("user", "I want my money back")))
			if first.status != http.StatusOK || first.content(t) != tc.wantAnswer {
				t.Fatalf("first reply = %d %q, want 200 %q", first.status, first.body, tc.wantAnswer)
			}
			if got := h.log.approvers(); !slices.Equal(got, tc.wantAsked) {
				t.Errorf("owner notified through %v, want %v", got, tc.wantAsked)
			}
			handoff, found, err := store.NewHandoffStore(h.db).Get(t.Context(), first.session)
			if err != nil || found != tc.wantOpened {
				t.Fatalf("handoff = %+v, found %v, %v; want found %v", handoff, found, err, tc.wantOpened)
			}
			next := h.post(key, chatJSON(chatMessage("user", "hello?")), httpchat.SessionHeader, first.session)
			if !tc.wantOpened {
				if next.status != http.StatusOK {
					t.Fatalf("next reply without a handoff = %d %s, want 200", next.status, next.body)
				}
				return
			}
			notified := h.log.first()
			wantPrincipal := "http:" + keyID
			if notified.requester.Principal != wantPrincipal || notified.session != first.session || notified.reason != handoffTurnReason {
				t.Errorf("owner notification = %+v, want principal %s for the session", notified, wantPrincipal)
			}
			if handoff.Channel != store.HandoffChannelHTTP || handoff.Account != keyID || handoff.PrincipalID != wantPrincipal || handoff.AgentID != "lobby" ||
				handoff.Reason != handoffTurnReason || handoff.ReplyChannelID != "" || handoff.ReplyThreadID != "" || !handoff.ReleasedAt.IsZero() {
				t.Errorf("handoff = %+v", handoff)
			}
			h.expectError(next, http.StatusConflict, "invalid_request_error", "handoff_open")
			if h.runtime.turns != 1 {
				t.Errorf("runtime ran %d turns, want the held session to stay out of the runtime", h.runtime.turns)
			}
		})
	}
}

func TestHTTPHandoffOwnerRepliesFlowThroughTheReadEndpoint(t *testing.T) {
	delivered := captureDeliveries(t)
	deliveredTelegram := captureTelegramDeliveries(t, nil)
	h := newHandoffHTTPHarness(t, func(log *askLog) []approvalAdapter {
		return []approvalAdapter{&scriptedApprover{name: "telegram", serves: "lobby", log: log}}
	})
	key, keyID := h.key("lobby", "public")
	otherKey, _ := h.key("lobby", "public")
	first := h.post(key, chatJSON(chatMessage("user", "I want my money back")))
	session := first.session

	status, listing := h.replies(key, session, "")
	if status != http.StatusOK || len(listing.Data) != 0 || !listing.HandoffOpen || listing.Object != "list" {
		t.Fatalf("replies before any owner reply = %d %+v, want an empty open list", status, listing)
	}

	listed, err := runAccessCommand(t, newHandoffCmd, h.config, "list")
	if err != nil || !strings.Contains(listed, session+"\thttp:"+keyID+"\tlobby\t") {
		t.Fatalf("handoff list = %q, %v", listed, err)
	}
	for _, text := range []string{"Your refund is approved.", "Anything else?"} {
		if _, err := runAccessCommand(t, newReplyCmd, h.config, session, text); err != nil {
			t.Fatalf("reply %q: %v", text, err)
		}
	}
	if len(*delivered) != 0 || len(*deliveredTelegram) != 0 {
		t.Fatalf("an http reply was delivered through a channel: discord %+v telegram %+v", *delivered, *deliveredTelegram)
	}
	if accessEventCount(t, h.config, accessActionHandoffReply) != 2 {
		t.Error("owner replies were not recorded as owner actions")
	}

	status, listing = h.replies(key, session, "")
	if status != http.StatusOK || !listing.HandoffOpen || len(listing.Data) != 2 ||
		listing.Data[0].Content != "Your refund is approved." || listing.Data[1].Content != "Anything else?" ||
		listing.Data[0].Object != "handoff.reply" {
		t.Fatalf("replies = %d %+v, want both owner replies oldest first", status, listing)
	}
	for _, item := range listing.Data {
		if strings.Contains(item.Content, handoffTurnHandedOff) {
			t.Errorf("a persona answer leaked into the replies: %+v", item)
		}
	}
	status, tail := h.replies(key, session, "?after="+listing.Data[0].ID)
	if status != http.StatusOK || len(tail.Data) != 1 || tail.Data[0].Content != "Anything else?" {
		t.Fatalf("replies after the first = %d %+v", status, tail)
	}
	if status, _ := h.replies(otherKey, session, ""); status != http.StatusNotFound {
		t.Fatalf("another key read the session: status %d, want 404", status)
	}

	if _, err := runAccessCommand(t, newHandoffCmd, h.config, "release", session); err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(*delivered) != 0 || len(*deliveredTelegram) != 0 {
		t.Fatalf("an http release notice was delivered through a channel: %+v %+v", *delivered, *deliveredTelegram)
	}
	status, released := h.replies(key, session, "")
	if status != http.StatusOK || released.HandoffOpen || len(released.Data) != 2 {
		t.Fatalf("replies after release = %d %+v, want closed with both replies", status, released)
	}
	resumed := h.post(key, chatJSON(chatMessage("user", "thanks")), httpchat.SessionHeader, session)
	if resumed.status != http.StatusOK {
		t.Fatalf("turn after release = %d %s, want 200", resumed.status, resumed.body)
	}
}

func TestHTTPHandoffRepliesRejectUnusableRequests(t *testing.T) {
	h := newHandoffHTTPHarness(t, func(log *askLog) []approvalAdapter {
		return []approvalAdapter{&scriptedApprover{name: "telegram", log: log}}
	})
	key, _ := h.key("lobby", "public")
	session := h.post(key, chatJSON(chatMessage("user", "hi"))).session
	cases := map[string]struct {
		key, session, query string
		want                int
	}{
		"missing session header": {key: key, want: http.StatusBadRequest},
		"bad cursor":             {key: key, session: session, query: "?after=latest", want: http.StatusBadRequest},
		"unknown session":        {key: key, session: strings.TrimSuffix(session, session[len(session)-1:]) + "z", want: http.StatusNotFound},
		"bad key":                {key: "aura-nonsense", session: session, want: http.StatusUnauthorized},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if status, _ := h.replies(tc.key, tc.session, tc.query); status != tc.want {
				t.Errorf("status = %d, want %d", status, tc.want)
			}
		})
	}
}

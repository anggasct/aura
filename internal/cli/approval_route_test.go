package cli

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channel/approval"
	"github.com/anggasct/aura/internal/channel/discord"
	"github.com/anggasct/aura/internal/channel/httpchat"
	"github.com/anggasct/aura/internal/channel/telegram"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/toolbroker"
)

type askedRequest struct {
	approver  string
	prompt    approval.Prompt
	requester approval.Requester
}

type askLog struct {
	mu    sync.Mutex
	asked []askedRequest
}

func (l *askLog) record(entry *askedRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, *entry)
}

func (l *askLog) approvers() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	names := make([]string, 0, len(l.asked))
	for i := range l.asked {
		names = append(names, l.asked[i].approver)
	}
	return names
}

func (l *askLog) first() askedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.asked) == 0 {
		return askedRequest{}
	}
	return l.asked[0]
}

type scriptedApprover struct {
	name   string
	serves string
	accept bool
	err    error
	log    *askLog
}

func (s *scriptedApprover) HoldsApprovalTarget(string) bool { return false }

func (s *scriptedApprover) Decide(context.Context, *approval.Prompt) (bool, error) {
	return false, nil
}

func (s *scriptedApprover) ServesPersona(persona string) bool {
	return s.serves != "" && s.serves == persona
}

func (s *scriptedApprover) DecideForOwner(_ context.Context, prompt *approval.Prompt, requester *approval.Requester) (bool, error) {
	s.log.record(&askedRequest{approver: s.name, prompt: *prompt, requester: *requester})
	return s.accept, s.err
}

type routedResults struct {
	mu      sync.Mutex
	results []string
}

func (r *routedResults) observe(_ context.Context, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, result)
}

func (r *routedResults) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.results)
}

func discordUnavailable() error {
	return discord.Errorf(discord.ErrorCodeApprovalUnavailable, "owner has no discord identity")
}

func telegramUnavailable() error {
	return telegram.Errorf(telegram.ErrorCodeApprovalUnavailable, "owner has no telegram identity")
}

func routedPrompt() *approval.Prompt {
	return &approval.Prompt{ToolName: "write_file", SessionID: "lobby/http/key/session", PrincipalID: "http:key", ExpiresAt: time.Now().Add(time.Minute)}
}

func routedRequester() *approval.Requester {
	return &approval.Requester{Principal: "http:key", Class: principal.ClassPublic, Persona: "lobby"}
}

func TestOwnerApprovalRouterOrdersAndFallsThrough(t *testing.T) {
	timeout := discord.Errorf(discord.ErrorCodeApprovalTimeout, "owner did not answer the approval in time")
	failure := errors.New("owner dm exploded")
	cases := []struct {
		name         string
		build        func(log *askLog) []ownerApprover
		wantAccepted bool
		wantErr      func(error) bool
		wantAsked    []string
	}{
		{
			name: "persona serving approver goes first",
			build: func(log *askLog) []ownerApprover {
				return []ownerApprover{
					&scriptedApprover{name: "discord", accept: false, log: log},
					&scriptedApprover{name: "telegram", serves: "lobby", accept: true, log: log},
				}
			},
			wantAccepted: true,
			wantAsked:    []string{"telegram"},
		},
		{
			name: "config order holds among approvers that serve the persona",
			build: func(log *askLog) []ownerApprover {
				return []ownerApprover{
					&scriptedApprover{name: "discord", serves: "lobby", accept: true, log: log},
					&scriptedApprover{name: "telegram", serves: "lobby", accept: false, log: log},
				}
			},
			wantAccepted: true,
			wantAsked:    []string{"discord"},
		},
		{
			name: "config order holds when none serves the persona",
			build: func(log *askLog) []ownerApprover {
				return []ownerApprover{
					&scriptedApprover{name: "discord", err: discordUnavailable(), log: log},
					&scriptedApprover{name: "telegram", accept: true, log: log},
				}
			},
			wantAccepted: true,
			wantAsked:    []string{"discord", "telegram"},
		},
		{
			name: "unavailable serving approver falls through to the rest",
			build: func(log *askLog) []ownerApprover {
				return []ownerApprover{
					&scriptedApprover{name: "discord", accept: true, log: log},
					&scriptedApprover{name: "telegram", serves: "lobby", err: telegramUnavailable(), log: log},
				}
			},
			wantAccepted: true,
			wantAsked:    []string{"telegram", "discord"},
		},
		{
			name: "a rejection is final",
			build: func(log *askLog) []ownerApprover {
				return []ownerApprover{
					&scriptedApprover{name: "discord", accept: false, log: log},
					&scriptedApprover{name: "telegram", accept: true, log: log},
				}
			},
			wantAsked: []string{"discord"},
		},
		{
			name: "a timeout is final",
			build: func(log *askLog) []ownerApprover {
				return []ownerApprover{
					&scriptedApprover{name: "discord", err: timeout, log: log},
					&scriptedApprover{name: "telegram", accept: true, log: log},
				}
			},
			wantErr:   func(err error) bool { return errors.Is(err, timeout) },
			wantAsked: []string{"discord"},
		},
		{
			name: "an uncoded failure is final",
			build: func(log *askLog) []ownerApprover {
				return []ownerApprover{
					&scriptedApprover{name: "discord", err: failure, log: log},
					&scriptedApprover{name: "telegram", accept: true, log: log},
				}
			},
			wantErr:   func(err error) bool { return errors.Is(err, failure) },
			wantAsked: []string{"discord"},
		},
		{
			name: "every approver unavailable denies",
			build: func(log *askLog) []ownerApprover {
				return []ownerApprover{
					&scriptedApprover{name: "discord", err: discordUnavailable(), log: log},
					&scriptedApprover{name: "telegram", err: telegramUnavailable(), log: log},
				}
			},
			wantErr:   approvalUnavailable,
			wantAsked: []string{"discord", "telegram"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &askLog{}
			routed := &routedResults{}
			router := &ownerApprovalRouter{approvers: tc.build(log), routed: routed.observe}
			accepted, err := router.DecideForOwner(t.Context(), routedPrompt(), routedRequester())
			if accepted != tc.wantAccepted {
				t.Errorf("accepted = %v, want %v", accepted, tc.wantAccepted)
			}
			if (tc.wantErr == nil) != (err == nil) || (tc.wantErr != nil && !tc.wantErr(err)) {
				t.Errorf("err = %v", err)
			}
			if got := log.approvers(); !slices.Equal(got, tc.wantAsked) {
				t.Errorf("asked = %v, want %v", got, tc.wantAsked)
			}
			if got := routed.recorded(); len(got) != 0 {
				t.Errorf("router recorded %v; adapters own their observations", got)
			}
		})
	}
}

func TestOwnerApprovalRouterWithoutApproversDeniesAndRecordsUnavailable(t *testing.T) {
	routed := &routedResults{}
	router := &ownerApprovalRouter{routed: routed.observe}
	accepted, err := router.DecideForOwner(t.Context(), routedPrompt(), routedRequester())
	if accepted || err != nil {
		t.Errorf("DecideForOwner() = %v, %v; want a plain denial", accepted, err)
	}
	if got := routed.recorded(); !slices.Equal(got, []string{approval.RoutedUnavailable}) {
		t.Errorf("routed observations = %v, want one unavailable", got)
	}
}

type decidingRuntime struct {
	decide toolbroker.ApprovalDecider
}

func (r decidingRuntime) Run(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		if !yield(store.RuntimeEvent{TurnID: "turn-1", Kind: runtime.EventKindTurnAccepted}, nil) {
			return
		}
		accepted, err := r.decide(ctx, &toolbroker.ApprovalPrompt{
			ToolName:    "write_file",
			ToolVersion: "1",
			SessionID:   req.SessionID,
			TurnID:      "turn-1",
			PrincipalID: req.PrincipalID,
			Arguments:   `{"path":"a.txt"}`,
			ReasonCode:  "approval_required",
			ExpiresAt:   time.Now().Add(time.Minute),
		})
		text := "the tool ran"
		if err != nil || !accepted {
			text = "the tool was denied"
		}
		answer := store.RuntimeEvent{TurnID: "turn-1", Kind: runtime.EventKindMessageCompleted}
		answer.Payload, _ = json.Marshal(map[string]string{"text": text})
		if !yield(answer, nil) {
			return
		}
		yield(store.RuntimeEvent{TurnID: "turn-1", Kind: runtime.EventKindTurnCompleted}, nil)
	}
}

func newApprovalHTTPHarness(t *testing.T, adapters []approvalAdapter, routed *routedResults) *httpChatHarness {
	t.Helper()
	path := writeHTTPChatRuntimeConfig(t, "50")
	cfg := mustLoadConfig(t, path)
	db := openAccessDB(t, path)
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	passEvals(t, db, registry, "lobby", "staff")
	approvals, err := newHTTPApprovals(adapters, routed.observe)
	if err != nil {
		t.Fatalf("newHTTPApprovals: %v", err)
	}
	decider := channelApprovalDecider(approvalChannels(adapters, approvals))
	server, _, err := buildHTTPChat(cfg, db, decidingRuntime{decide: decider}, registry, nil, approvals, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("buildHTTPChat: %v", err)
	}
	return &httpChatHarness{t: t, config: path, baseURL: serveHTTPChat(t, server), db: db}
}

func (h *httpChatHarness) ownerKey() (plaintext, id string) {
	h.t.Helper()
	stdout, _, err := runAPIKey(h.t, h.config, "create", "--persona", "lobby", "--class", "owner", "--confirm-owner", "--label", "test")
	if err != nil {
		h.t.Fatalf("apikey create: %v", err)
	}
	plaintext = strings.TrimSpace(stdout)
	id, err = httpchat.ParseKey(plaintext)
	if err != nil {
		h.t.Fatalf("ParseKey: %v", err)
	}
	return plaintext, id
}

func TestHTTPTurnApprovalsReachTheOwnersChat(t *testing.T) {
	cases := []struct {
		name          string
		class         string
		build         func(log *askLog) []approvalAdapter
		wantAnswer    string
		wantAsked     []string
		wantPrincipal func(keyID string) string
		wantClass     principal.Class
	}{
		{
			name:  "public key accepted by the persona serving adapter",
			class: "public",
			build: func(log *askLog) []approvalAdapter {
				return []approvalAdapter{
					&scriptedApprover{name: "discord", log: log},
					&scriptedApprover{name: "telegram", serves: "lobby", accept: true, log: log},
				}
			},
			wantAnswer:    "the tool ran",
			wantAsked:     []string{"telegram"},
			wantPrincipal: func(keyID string) string { return "http:" + keyID },
			wantClass:     principal.ClassPublic,
		},
		{
			name:  "trusted key rejected by the owner",
			class: "trusted",
			build: func(log *askLog) []approvalAdapter {
				return []approvalAdapter{&scriptedApprover{name: "discord", serves: "lobby", accept: false, log: log}}
			},
			wantAnswer:    "the tool was denied",
			wantAsked:     []string{"discord"},
			wantPrincipal: func(keyID string) string { return "http:" + keyID },
			wantClass:     principal.ClassTrusted,
		},
		{
			name:  "unavailable adapter falls through to the next",
			class: "public",
			build: func(log *askLog) []approvalAdapter {
				return []approvalAdapter{
					&scriptedApprover{name: "discord", serves: "lobby", err: discordUnavailable(), log: log},
					&scriptedApprover{name: "telegram", accept: true, log: log},
				}
			},
			wantAnswer:    "the tool ran",
			wantAsked:     []string{"discord", "telegram"},
			wantPrincipal: func(keyID string) string { return "http:" + keyID },
			wantClass:     principal.ClassPublic,
		},
		{
			name:  "every adapter unavailable denies the tool",
			class: "public",
			build: func(log *askLog) []approvalAdapter {
				return []approvalAdapter{
					&scriptedApprover{name: "discord", err: discordUnavailable(), log: log},
					&scriptedApprover{name: "telegram", err: telegramUnavailable(), log: log},
				}
			},
			wantAnswer:    "the tool was denied",
			wantAsked:     []string{"discord", "telegram"},
			wantPrincipal: func(keyID string) string { return "http:" + keyID },
			wantClass:     principal.ClassPublic,
		},
		{
			name:  "owner key routes the same way",
			class: "owner",
			build: func(log *askLog) []approvalAdapter {
				return []approvalAdapter{&scriptedApprover{name: "telegram", serves: "lobby", accept: true, log: log}}
			},
			wantAnswer:    "the tool ran",
			wantAsked:     []string{"telegram"},
			wantPrincipal: func(string) string { return principal.OwnerID },
			wantClass:     principal.ClassOwner,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &askLog{}
			h := newApprovalHTTPHarness(t, tc.build(log), &routedResults{})
			var key, keyID string
			if tc.class == "owner" {
				key, keyID = h.ownerKey()
			} else {
				key, keyID = h.key("lobby", tc.class)
			}
			reply := h.post(key, chatJSON(chatMessage("user", "write the file")))
			if reply.status != http.StatusOK || reply.content(t) != tc.wantAnswer {
				t.Fatalf("reply = %d %q, want 200 %q", reply.status, reply.body, tc.wantAnswer)
			}
			if got := log.approvers(); !slices.Equal(got, tc.wantAsked) {
				t.Errorf("asked = %v, want %v", got, tc.wantAsked)
			}
			first := log.first()
			wantRequester := approval.Requester{Principal: tc.wantPrincipal(keyID), Class: tc.wantClass, Persona: "lobby"}
			if first.requester != wantRequester || first.prompt.PrincipalID != wantRequester.Principal || first.prompt.ToolName != "write_file" {
				t.Errorf("first request = %+v, want requester %+v for write_file", first, wantRequester)
			}
		})
	}
}

func TestHTTPTurnWithoutOwnerChannelsIsDeniedAndRecorded(t *testing.T) {
	routed := &routedResults{}
	h := newApprovalHTTPHarness(t, nil, routed)
	key, _ := h.key("lobby", "public")
	reply := h.post(key, chatJSON(chatMessage("user", "write the file")))
	if reply.status != http.StatusOK || reply.content(t) != "the tool was denied" {
		t.Fatalf("reply = %d %q, want a completed turn with the tool denied", reply.status, reply.body)
	}
	if got := routed.recorded(); !slices.Equal(got, []string{approval.RoutedUnavailable}) {
		t.Errorf("routed observations = %v, want one unavailable", got)
	}
}

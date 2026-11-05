package telegram

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
)

const (
	allowedUserID = 555001
	grantedUserID = 555002
	publicUserID  = 555003
)

type scopeLog struct {
	mu     sync.Mutex
	scopes []auraagent.RouteScope
}

func (l *scopeLog) record(scope *auraagent.RouteScope) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.scopes = append(l.scopes, *scope)
}

func (l *scopeLog) recorded() []auraagent.RouteScope {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.scopes)
}

type accessRouter struct {
	definition auraagent.Definition
	public     bool
	unrouted   map[string]bool
	scopes     *scopeLog
}

func (r accessRouter) Resolve(scope *auraagent.RouteScope) (auraagent.RouteMatch, bool) {
	if r.scopes != nil {
		r.scopes.record(scope)
	}
	if r.unrouted[scope.UserID] {
		return auraagent.RouteMatch{}, false
	}
	return fixedRouter{}.Resolve(scope)
}

func (r accessRouter) Persona(id string) (auraagent.Definition, bool) {
	return r.definition, id == r.definition.ID
}

func (r accessRouter) ServesPublic(channel, account string) bool {
	return r.public && channel == config.BindingChannelTelegram && account == "pocket"
}

type fakePairing struct {
	mu        sync.Mutex
	granted   map[string]bool
	result    access.PairingResult
	requestEr error
	grantEr   error
	requests  []string
}

func (f *fakePairing) Granted(_ context.Context, principalID, account string) (bool, error) {
	if f.grantEr != nil {
		return false, f.grantEr
	}
	return f.granted[principalID+"@"+account], nil
}

func (f *fakePairing) Request(_ context.Context, principalID, account string) (access.PairingResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, principalID+"@"+account)
	return f.result, f.requestEr
}

func (f *fakePairing) requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

type budgetCall struct {
	principalID string
	class       principal.Class
}

type fakeBudgets struct {
	mu      sync.Mutex
	refusal string
	err     error
	calls   []budgetCall
}

func (f *fakeBudgets) Admit(_ context.Context, principalID string, class principal.Class) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, budgetCall{principalID, class})
	return f.refusal, f.err
}

func (f *fakeBudgets) recorded() []budgetCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

type fakeEvals struct {
	evaluated bool
	err       error
	personas  []string
}

func (f *fakeEvals) Evaluated(_ context.Context, persona string) (bool, error) {
	f.personas = append(f.personas, persona)
	return f.evaluated, f.err
}

type fakeHandoffs struct {
	state    string
	err      error
	sessions []string
}

func (f *fakeHandoffs) Check(_ context.Context, sessionID string) (string, error) {
	f.sessions = append(f.sessions, sessionID)
	return f.state, f.err
}

type fakeEraser struct {
	mu        sync.Mutex
	err       error
	forgotten []string
}

func (f *fakeEraser) Forget(_ context.Context, principalID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, principalID)
	return f.err
}

func (f *fakeEraser) erased() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.forgotten)
}

type accessOpts struct {
	allowed       []string
	chats         []string
	privateOff    bool
	definition    auraagent.Definition
	public        bool
	unrouted      []string
	settings      AccessSettings
	withoutAccess bool
}

type accessRig struct {
	scopes  *scopeLog
	h       *harness
	adapter *Adapter
	sink    *memorySink
	mu      sync.Mutex
	drops   []string
}

func newAccessRig(t *testing.T, opts accessOpts) *accessRig {
	t.Helper()
	h := newHarness(t)
	return assembleRig(t, h, opts, h.cursors, h.effects, h.sessions)
}

func assembleRig(t *testing.T, h *harness, opts accessOpts, cursors CursorStore, effects EffectRunner, sessions SessionEnsurer) *accessRig {
	t.Helper()
	if opts.definition.ID == "" {
		opts.definition = auraagent.Definition{ID: testPersona, Name: "Main", Audience: config.AudiencePublic}
	}
	cfg := &config.Telegram{
		Enabled:            true,
		Polling:            config.TelegramPolling{Timeout: config.Duration(time.Second), MaxBackoff: config.Duration(time.Second)},
		MaxAttachmentBytes: config.TelegramMaxAttachmentBytes,
	}
	account := &config.TelegramAccount{
		Name:               "pocket",
		BotTokenRef:        "env://" + testTokenEnv,
		AllowedUserIDs:     opts.allowed,
		AllowedChatIDs:     opts.chats,
		AcceptPrivateChats: !opts.privateOff,
	}
	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	adapter, err := New(cfg, account, cursors, effects, sessions, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	owners, err := principal.NewResolver([]string{"telegram:" + strconv.Itoa(ownerUserID)})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	rig := &accessRig{h: h, adapter: adapter, sink: newMemorySink(), scopes: &scopeLog{}}
	unrouted := map[string]bool{}
	for _, id := range opts.unrouted {
		unrouted[id] = true
	}
	adapter.SetPrincipalResolver(owners)
	adapter.SetPersonaRouter(accessRouter{definition: opts.definition, public: opts.public, unrouted: unrouted, scopes: rig.scopes})
	if !opts.withoutAccess {
		adapter.SetAccess(&opts.settings)
	}
	adapter.SetDenialObserver(func(_ context.Context, reason string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.denials = append(h.denials, reason)
	})
	adapter.SetDropObserver(func(_ context.Context, reason string) {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.drops = append(rig.drops, reason)
	})
	adapter.bot.base = h.api.srv.URL
	adapter.retryBase = 10 * time.Millisecond
	adapter.rateCap = 50 * time.Millisecond
	return rig
}

func (r *accessRig) run(t *testing.T, updates ...update) {
	t.Helper()
	r.h.api.queue(updates...)
	stop := startAdapter(t, r.adapter, r.sink)
	defer stop()
	awaitCursor(t, r.h, updates[len(updates)-1].UpdateID+1)
}

func (r *accessRig) dropped() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.drops)
}

func (r *accessRig) onlyEnvelope(t *testing.T) (principalID, class, conversation string) {
	t.Helper()
	accepted := r.sink.accepted()
	if len(accepted) != 1 {
		t.Fatalf("accepted %d envelopes, want 1", len(accepted))
	}
	return accepted[0].PrincipalID, accepted[0].PrincipalClass, accepted[0].ConversationID
}

func (r *accessRig) assertQuiet(t *testing.T, wantDrops ...string) {
	t.Helper()
	if accepted := r.sink.accepted(); len(accepted) != 0 {
		t.Fatalf("accepted %d envelopes, want none", len(accepted))
	}
	if got := r.dropped(); !slices.Equal(got, wantDrops) {
		t.Fatalf("drop records = %v, want %v", got, wantDrops)
	}
}

func sentTo(api *fakeBotAPI, chatID int64) []string {
	var texts []string
	for _, body := range api.sends() {
		if id, _ := body["chat_id"].(float64); int64(id) == chatID {
			text, _ := body["text"].(string)
			texts = append(texts, text)
		}
	}
	return texts
}

func personaMessage(key string) string {
	definition := auraagent.Definition{}
	return definition.Message(key)
}

func TestAdmissionClassesMatchTheSharedResolver(t *testing.T) {
	granted := &fakePairing{granted: map[string]bool{"telegram:" + strconv.Itoa(grantedUserID) + "@pocket": true}}
	cases := []struct {
		name      string
		userID    int64
		allowed   []string
		public    bool
		wantID    string
		wantClass principal.Class
		wantDrops []string
		wantDeny  []string
	}{
		{name: "owner", userID: ownerUserID, wantID: principal.OwnerID, wantClass: principal.ClassOwner},
		{name: "owner that is also allowlisted", userID: ownerUserID, allowed: []string{strconv.Itoa(ownerUserID)}, wantID: principal.OwnerID, wantClass: principal.ClassOwner},
		{name: "allowlisted", userID: allowedUserID, allowed: []string{strconv.Itoa(allowedUserID)}, wantID: "telegram:555001", wantClass: principal.ClassTrusted},
		{name: "pairing granted", userID: grantedUserID, wantID: "telegram:555002", wantClass: principal.ClassTrusted},
		{name: "served public", userID: publicUserID, public: true, wantID: "telegram:555003", wantClass: principal.ClassPublic},
		{name: "unknown on an account that does not serve public", userID: strangerID, wantDrops: []string{"author_not_allowed"}, wantDeny: []string{DeniedReasonIdentity}},
		{name: "allowlisted wins over public", userID: allowedUserID, allowed: []string{strconv.Itoa(allowedUserID)}, public: true, wantID: "telegram:555001", wantClass: principal.ClassTrusted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAccessRig(t, accessOpts{allowed: tc.allowed, public: tc.public, settings: AccessSettings{Pairing: granted}})
			rig.run(t, privateText(10, tc.userID, "hello"))
			if tc.wantID == "" {
				rig.assertQuiet(t, tc.wantDrops...)
				if got := rig.h.denied(); !slices.Equal(got, tc.wantDeny) {
					t.Fatalf("denials = %v, want %v", got, tc.wantDeny)
				}
				return
			}
			id, class, conversation := rig.onlyEnvelope(t)
			if id != tc.wantID || class != string(tc.wantClass) {
				t.Errorf("principal = %q %q, want %q %q", id, class, tc.wantID, tc.wantClass)
			}
			if want := "main/telegram:pocket/chat:" + strconv.FormatInt(tc.userID, 10); conversation != want {
				t.Errorf("conversation = %q, want %q", conversation, want)
			}
			if got := rig.dropped(); len(got) != 0 {
				t.Errorf("admitted message produced drop records %v", got)
			}
		})
	}
}

func TestAdmissionDefaultsStayOwnerOnly(t *testing.T) {
	rig := newAccessRig(t, accessOpts{withoutAccess: true})
	rig.run(t, privateText(10, ownerUserID, "hello"), privateText(11, strangerID, "let me in"))
	if id, class, _ := rig.onlyEnvelope(t); id != principal.OwnerID || class != string(principal.ClassOwner) {
		t.Fatalf("admitted %q %q, want only the owner", id, class)
	}
	if got := rig.dropped(); !slices.Equal(got, []string{"author_not_allowed"}) {
		t.Errorf("drops = %v", got)
	}
	if got := rig.h.denied(); !slices.Equal(got, []string{DeniedReasonIdentity}) {
		t.Errorf("denials = %v", got)
	}
}

func TestRoutingGateDropsUnroutedSender(t *testing.T) {
	rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, unrouted: []string{strconv.Itoa(allowedUserID)}})
	rig.run(t, privateText(10, allowedUserID, "hello"))
	rig.assertQuiet(t, "persona_unrouted")
	if sends := rig.h.api.sends(); len(sends) != 0 {
		t.Errorf("unrouted sender received %d replies", len(sends))
	}
}

func TestAudienceGateDropsClassesThePersonaDoesNotAdmit(t *testing.T) {
	trusted := auraagent.Definition{ID: testPersona, Audience: config.AudienceTrusted}
	rig := newAccessRig(t, accessOpts{definition: trusted, public: true})
	rig.run(t, privateText(10, publicUserID, "hello"))
	rig.assertQuiet(t, "audience")
	if got := rig.h.denied(); !slices.Equal(got, []string{access.DeniedReasonAudience}) {
		t.Errorf("denials = %v, want one audience denial", got)
	}
	if sends := rig.h.api.sends(); len(sends) != 0 {
		t.Errorf("audience drop replied %d times", len(sends))
	}
}

func TestPairingRepliesToUnknownSendersWithTheIssuedCode(t *testing.T) {
	pairing := &fakePairing{result: access.PairingResult{Outcome: access.PairingOutcomeIssued, Code: "ABCD2345"}}
	definition := auraagent.Definition{ID: testPersona, Audience: config.AudienceTrusted, Pairing: true}
	rig := newAccessRig(t, accessOpts{definition: definition, settings: AccessSettings{Pairing: pairing}})
	rig.run(t, privateText(10, strangerID, "/start"))
	rig.assertQuiet(t, "pairing")
	if got := pairing.requested(); !slices.Equal(got, []string{"telegram:999001@pocket"}) {
		t.Fatalf("pairing requests = %v", got)
	}
	want := strings.ReplaceAll(personaMessage(config.MessagePairingCode), config.PairingCodePlaceholder, "ABCD2345")
	if got := sentTo(rig.h.api, strangerID); !slices.Equal(got, []string{want}) {
		t.Fatalf("pairing reply = %q, want %q", got, want)
	}
	if got := rig.h.denied(); len(got) != 0 {
		t.Errorf("a pairing candidate was also counted as an identity denial: %v", got)
	}
	if logs := rig.h.logs.String(); strings.Contains(logs, "ABCD2345") || strings.Contains(logs, "999001") {
		t.Errorf("logs carry the pairing code or the sender id: %s", logs)
	}
}

func TestPairingOutcomesPickTheMatchingMessage(t *testing.T) {
	definition := auraagent.Definition{ID: testPersona, Audience: config.AudienceTrusted, Pairing: true}
	cases := map[string]struct {
		result access.PairingResult
		err    error
		want   string
	}{
		"pending": {result: access.PairingResult{Outcome: access.PairingOutcomePending}, want: personaMessage(config.MessagePairingPending)},
		"locked":  {result: access.PairingResult{Outcome: access.PairingOutcomeLocked}, want: personaMessage(config.MessagePairingLocked)},
		"failed":  {err: errors.New("store down")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			pairing := &fakePairing{result: tc.result, requestEr: tc.err}
			rig := newAccessRig(t, accessOpts{definition: definition, settings: AccessSettings{Pairing: pairing}})
			rig.run(t, privateText(10, strangerID, "hi"))
			rig.assertQuiet(t, "pairing")
			got := sentTo(rig.h.api, strangerID)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("failed pairing request replied %q", got)
				}
				return
			}
			if !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("reply = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPairingFallsBackWhenTheAudienceRefusesAPublicSender(t *testing.T) {
	pairing := &fakePairing{result: access.PairingResult{Outcome: access.PairingOutcomeIssued, Code: "ZXCV5678"}}
	definition := auraagent.Definition{ID: testPersona, Audience: config.AudienceTrusted, Pairing: true}
	rig := newAccessRig(t, accessOpts{definition: definition, public: true, settings: AccessSettings{Pairing: pairing}})
	rig.run(t, privateText(10, publicUserID, "hi"))
	rig.assertQuiet(t, "pairing")
	if got := sentTo(rig.h.api, publicUserID); len(got) != 1 || !strings.Contains(got[0], "ZXCV5678") {
		t.Fatalf("pairing reply = %q", got)
	}
}

func TestPairingNeverStartsFromAPersonaWithoutPairing(t *testing.T) {
	pairing := &fakePairing{result: access.PairingResult{Outcome: access.PairingOutcomeIssued, Code: "NOPE0000"}}
	for name, definition := range map[string]auraagent.Definition{
		"pairing off":       {ID: testPersona, Audience: config.AudienceTrusted},
		"audience is owner": {ID: testPersona, Audience: config.AudienceOwner, Pairing: true},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t, accessOpts{definition: definition, settings: AccessSettings{Pairing: pairing}})
			rig.run(t, privateText(10, strangerID, "hi"))
			rig.assertQuiet(t, "author_not_allowed")
			if sends := rig.h.api.sends(); len(sends) != 0 {
				t.Errorf("replied %d times", len(sends))
			}
		})
	}
	if got := pairing.requested(); len(got) != 0 {
		t.Errorf("pairing requests = %v, want none", got)
	}
}

func TestPairingGrantAdmitsATrustedSender(t *testing.T) {
	pairing := &fakePairing{granted: map[string]bool{"telegram:999001@pocket": true}}
	definition := auraagent.Definition{ID: testPersona, Audience: config.AudienceTrusted, Pairing: true}
	rig := newAccessRig(t, accessOpts{definition: definition, settings: AccessSettings{Pairing: pairing}})
	rig.run(t, privateText(10, strangerID, "hi"))
	if id, class, _ := rig.onlyEnvelope(t); id != "telegram:999001" || class != string(principal.ClassTrusted) {
		t.Fatalf("principal = %q %q", id, class)
	}
	if got := pairing.requested(); len(got) != 0 {
		t.Errorf("a granted sender triggered pairing requests %v", got)
	}
}

func TestPairingGrantLookupFailureFailsClosed(t *testing.T) {
	pairing := &fakePairing{grantEr: errors.New("store down")}
	rig := newAccessRig(t, accessOpts{settings: AccessSettings{Pairing: pairing}})
	rig.run(t, privateText(10, strangerID, "hi"))
	rig.assertQuiet(t, "author_not_allowed")
}

func TestPrivateChatsDisabledRejectsEveryNonOwnerWithoutPairing(t *testing.T) {
	pairing := &fakePairing{
		granted: map[string]bool{"telegram:555002@pocket": true},
		result:  access.PairingResult{Outcome: access.PairingOutcomeIssued, Code: "ABCD2345"},
	}
	definition := auraagent.Definition{ID: testPersona, Audience: config.AudienceTrusted, Pairing: true}
	rig := newAccessRig(t, accessOpts{
		allowed:    []string{strconv.Itoa(allowedUserID)},
		privateOff: true,
		definition: definition,
		settings:   AccessSettings{Pairing: pairing},
	})
	rig.run(t,
		privateText(10, ownerUserID, "owner still passes"),
		privateText(11, allowedUserID, "allowlisted"),
		privateText(12, grantedUserID, "granted"),
		privateText(13, strangerID, "unknown"),
	)
	if id, class, _ := rig.onlyEnvelope(t); id != principal.OwnerID || class != string(principal.ClassOwner) {
		t.Fatalf("admitted %q %q, want the owner only", id, class)
	}
	want := []string{"private_chats_disabled", "private_chats_disabled", "author_not_allowed"}
	if got := rig.dropped(); !slices.Equal(got, want) {
		t.Fatalf("drops = %v, want %v", got, want)
	}
	if got := pairing.requested(); len(got) != 0 {
		t.Errorf("pairing started with private chats disabled: %v", got)
	}
	if sends := rig.h.api.sends(); len(sends) != 0 {
		t.Errorf("replied %d times with private chats disabled", len(sends))
	}
}

func TestEvalGateRefusesNonOwnersOfUnevaluatedPersonas(t *testing.T) {
	cases := map[string]struct {
		evals     *fakeEvals
		wantDrops []string
		wantReply bool
	}{
		"not evaluated": {evals: &fakeEvals{}, wantDrops: []string{"persona_eval_missing"}, wantReply: true},
		"lookup fails":  {evals: &fakeEvals{err: errors.New("store down")}, wantDrops: []string{"persona_eval_missing"}, wantReply: true},
		"evaluated":     {evals: &fakeEvals{evaluated: true}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Evals: tc.evals}})
			rig.run(t, privateText(10, allowedUserID, "hello"))
			if len(tc.wantDrops) == 0 {
				rig.onlyEnvelope(t)
				return
			}
			rig.assertQuiet(t, tc.wantDrops...)
			if got := rig.h.denied(); !slices.Equal(got, []string{access.DeniedReasonEvalMissing}) {
				t.Errorf("denials = %v", got)
			}
			want := personaMessage(config.MessageUnavailable)
			if got := sentTo(rig.h.api, allowedUserID); !slices.Equal(got, []string{want}) {
				t.Errorf("notice = %q, want %q", got, want)
			}
		})
	}
}

func TestEvalGateSkipsOwnersAndOwnerOnlyPersonas(t *testing.T) {
	evals := &fakeEvals{}
	rig := newAccessRig(t, accessOpts{settings: AccessSettings{Evals: evals}})
	rig.run(t, privateText(10, ownerUserID, "hello"))
	rig.onlyEnvelope(t)
	ownerOnly := auraagent.Definition{ID: testPersona, Audience: config.AudienceOwner}
	other := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, definition: ownerOnly, settings: AccessSettings{Evals: evals}})
	other.run(t, privateText(10, allowedUserID, "hello"))
	other.assertQuiet(t, "audience")
	if len(evals.personas) != 0 {
		t.Errorf("eval gate consulted for %v, want no lookups", evals.personas)
	}
}

func TestBudgetGateRefusesWithThePersonaMessage(t *testing.T) {
	cases := map[string]struct {
		budgets   *fakeBudgets
		wantDrop  string
		wantReply string
	}{
		"rate limited": {budgets: &fakeBudgets{refusal: access.BudgetRefusalRateLimited}, wantDrop: "rate_limited", wantReply: personaMessage(config.MessageRateLimited)},
		"exhausted":    {budgets: &fakeBudgets{refusal: access.BudgetRefusalExhausted}, wantDrop: "budget_exhausted", wantReply: personaMessage(config.MessageBudgetExhausted)},
		"check fails":  {budgets: &fakeBudgets{err: errors.New("ledger down")}, wantDrop: "budget_check_failed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Budgets: tc.budgets}})
			rig.run(t, privateText(10, allowedUserID, "hello"))
			rig.assertQuiet(t, tc.wantDrop)
			if calls := tc.budgets.recorded(); len(calls) != 1 || calls[0] != (budgetCall{"telegram:555001", principal.ClassTrusted}) {
				t.Errorf("budget calls = %+v", calls)
			}
			got := sentTo(rig.h.api, allowedUserID)
			if tc.wantReply == "" {
				if len(got) != 0 {
					t.Errorf("a failed budget check replied %q", got)
				}
				return
			}
			if !slices.Equal(got, []string{tc.wantReply}) {
				t.Errorf("reply = %q, want %q", got, tc.wantReply)
			}
		})
	}
}

func TestBudgetGateAdmitsWithinLimitsAndSkipsOwners(t *testing.T) {
	budgets := &fakeBudgets{}
	rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Budgets: budgets}})
	rig.run(t, privateText(10, ownerUserID, "owner"), privateText(11, allowedUserID, "trusted"))
	if accepted := rig.sink.accepted(); len(accepted) != 2 {
		t.Fatalf("accepted %d envelopes, want 2", len(accepted))
	}
	if calls := budgets.recorded(); len(calls) != 1 || calls[0].principalID != "telegram:555001" {
		t.Errorf("budget calls = %+v, want only the trusted sender", calls)
	}
}

func TestHandoffGate(t *testing.T) {
	t.Run("open handoff forwards to the owner and holds the turn", func(t *testing.T) {
		handoffs := &fakeHandoffs{state: access.HandoffStateOpen}
		rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Handoffs: handoffs}})
		rig.run(t, privateText(10, allowedUserID, "need a human"))
		rig.assertQuiet(t)
		forwarded := sentTo(rig.h.api, ownerUserID)
		if len(forwarded) != 1 || !strings.Contains(forwarded[0], "need a human") || !strings.Contains(forwarded[0], "telegram:555001") || !strings.Contains(forwarded[0], "main/telegram:pocket/chat:555001") {
			t.Fatalf("forward = %q", forwarded)
		}
		if got := sentTo(rig.h.api, allowedUserID); len(got) != 0 {
			t.Errorf("the requester received %q", got)
		}
		if !slices.Equal(handoffs.sessions, []string{"main/telegram:pocket/chat:555001"}) {
			t.Errorf("handoff checked sessions %v", handoffs.sessions)
		}
	})
	t.Run("expired handoff tells the sender and continues", func(t *testing.T) {
		rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Handoffs: &fakeHandoffs{state: access.HandoffStateExpired}}})
		rig.run(t, privateText(10, allowedUserID, "hello again"))
		rig.onlyEnvelope(t)
		if got := sentTo(rig.h.api, allowedUserID); !slices.Equal(got, []string{personaMessage(config.MessageHandoffClosed)}) {
			t.Errorf("notice = %q", got)
		}
	})
	t.Run("no handoff continues untouched", func(t *testing.T) {
		rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Handoffs: &fakeHandoffs{state: access.HandoffStateNone}}})
		rig.run(t, privateText(10, allowedUserID, "hello"))
		rig.onlyEnvelope(t)
		if sends := rig.h.api.sends(); len(sends) != 0 {
			t.Errorf("replied %d times", len(sends))
		}
	})
	t.Run("a failed check holds the turn", func(t *testing.T) {
		rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Handoffs: &fakeHandoffs{err: errors.New("store down")}}})
		rig.run(t, privateText(10, allowedUserID, "hello"))
		rig.assertQuiet(t, "handoff_check_failed")
	})
	t.Run("the owner is never held", func(t *testing.T) {
		handoffs := &fakeHandoffs{state: access.HandoffStateOpen}
		rig := newAccessRig(t, accessOpts{settings: AccessSettings{Handoffs: handoffs}})
		rig.run(t, privateText(10, ownerUserID, "hello"))
		rig.onlyEnvelope(t)
		if len(handoffs.sessions) != 0 {
			t.Errorf("handoff gate consulted for the owner: %v", handoffs.sessions)
		}
	})
}

func TestHandoffForwardWithoutATelegramOwnerIsDropped(t *testing.T) {
	rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Handoffs: &fakeHandoffs{state: access.HandoffStateOpen}}})
	owners, err := principal.NewResolver(nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	rig.adapter.SetPrincipalResolver(owners)
	rig.run(t, privateText(10, allowedUserID, "hello"))
	rig.assertQuiet(t, "handoff_forward_failed")
	if sends := rig.h.api.sends(); len(sends) != 0 {
		t.Errorf("replied %d times", len(sends))
	}
}

func TestForgetMeCommand(t *testing.T) {
	t.Run("without confirmation asks for it", func(t *testing.T) {
		eraser := &fakeEraser{}
		rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Erasure: eraser}})
		rig.run(t, privateText(10, allowedUserID, "/forgetme"))
		rig.assertQuiet(t)
		if got := sentTo(rig.h.api, allowedUserID); !slices.Equal(got, []string{personaMessage(config.MessageForgetmeConfirm)}) {
			t.Errorf("reply = %q", got)
		}
		if erased := eraser.erased(); len(erased) != 0 {
			t.Errorf("erased %v without confirmation", erased)
		}
	})
	t.Run("with confirmation erases the sender", func(t *testing.T) {
		eraser := &fakeEraser{}
		rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Erasure: eraser}})
		rig.run(t, privateText(10, allowedUserID, "/forgetme CONFIRM"))
		rig.assertQuiet(t)
		if erased := eraser.erased(); !slices.Equal(erased, []string{"telegram:555001"}) {
			t.Errorf("erased %v", erased)
		}
		if got := sentTo(rig.h.api, allowedUserID); !slices.Equal(got, []string{personaMessage(config.MessageForgetmeDone)}) {
			t.Errorf("reply = %q", got)
		}
	})
	t.Run("a failed erasure sends no completion notice", func(t *testing.T) {
		eraser := &fakeEraser{err: errors.New("disk full")}
		rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Erasure: eraser}})
		rig.run(t, privateText(10, allowedUserID, "/forgetme confirm"))
		rig.assertQuiet(t)
		if got := sentTo(rig.h.api, allowedUserID); len(got) != 0 {
			t.Errorf("reply = %q", got)
		}
	})
	t.Run("the owner is not permitted", func(t *testing.T) {
		eraser := &fakeEraser{}
		rig := newAccessRig(t, accessOpts{settings: AccessSettings{Erasure: eraser}})
		rig.run(t, privateText(10, ownerUserID, "/forgetme confirm"))
		rig.assertQuiet(t)
		if got := sentTo(rig.h.api, ownerUserID); !slices.Equal(got, []string{personaMessage(config.MessageNotPermitted)}) {
			t.Errorf("reply = %q", got)
		}
		if erased := eraser.erased(); len(erased) != 0 {
			t.Errorf("erased the owner: %v", erased)
		}
	})
	t.Run("other slash text goes to the model", func(t *testing.T) {
		rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, settings: AccessSettings{Erasure: &fakeEraser{}}})
		rig.run(t, privateText(10, allowedUserID, "/forgetmenot"), privateText(11, allowedUserID, "please /forgetme"))
		if accepted := rig.sink.accepted(); len(accepted) != 2 {
			t.Fatalf("accepted %d envelopes, want 2", len(accepted))
		}
	})
	t.Run("without an erasure service the text is not intercepted", func(t *testing.T) {
		rig := newAccessRig(t, accessOpts{withoutAccess: true})
		rig.run(t, privateText(10, ownerUserID, "/forgetme"))
		rig.onlyEnvelope(t)
	})
}

func TestEveryDroppedMessageProducesOneReasonedRecordWithoutContentOrIDs(t *testing.T) {
	definition := auraagent.Definition{ID: testPersona, Audience: config.AudienceTrusted}
	rig := newAccessRig(t, accessOpts{
		allowed:    []string{strconv.Itoa(allowedUserID)},
		definition: definition,
		public:     true,
		unrouted:   []string{"555004"},
		settings:   AccessSettings{Evals: &fakeEvals{}},
	})
	group := privateText(13, ownerUserID, "secret group text")
	group.Message.Chat = chat{ID: -100500, Type: "supergroup"}
	fromBot := privateText(12, ownerUserID+1, "secret bot text")
	fromBot.Message.From.IsBot = true
	anonymous := privateText(14, ownerUserID, "secret anonymous text")
	anonymous.Message.From = nil
	rig.run(t,
		privateText(10, strangerID, "secret stranger text"),
		privateText(11, publicUserID, "secret public text"),
		fromBot,
		group,
		anonymous,
		privateText(15, ownerUserID, "   "),
		privateText(16, 555004, "secret unrouted text"),
		privateText(17, allowedUserID, "secret allowlisted text"),
		update{UpdateID: 18},
	)
	want := []string{"audience", "audience", "author_bot", "chat_not_supported", "author_missing", "empty_content", "persona_unrouted", "persona_eval_missing"}
	got := rig.dropped()
	if !slices.Equal(got, want) {
		t.Fatalf("drop records = %v, want %v", got, want)
	}
	logs := rig.h.logs.String()
	if count := strings.Count(logs, "telegram message dropped"); count != len(got) {
		t.Errorf("%d drop log lines for %d drop records", count, len(got))
	}
	for _, leak := range []string{"secret", "999001", "555001", "555002", "555003", "555004", "424242", "-100500"} {
		if strings.Contains(logs, leak) {
			t.Errorf("logs carry %q: %s", leak, logs)
		}
	}
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "telegram message dropped") && !strings.Contains(line, "component=telegram") {
			t.Errorf("drop log line lacks the component: %s", line)
		}
	}
}

func TestDropLevelSeparatesKnownFromUnknownSenders(t *testing.T) {
	rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}, unrouted: []string{strconv.Itoa(allowedUserID)}})
	rig.run(t, privateText(10, strangerID, "hi"), privateText(11, allowedUserID, "hi"))
	var unknown, known string
	for _, line := range strings.Split(rig.h.logs.String(), "\n") {
		switch {
		case strings.Contains(line, "reason=author_not_allowed"):
			unknown = line
		case strings.Contains(line, "reason=persona_unrouted"):
			known = line
		}
	}
	if !strings.Contains(unknown, "level=DEBUG") || !strings.Contains(known, "level=INFO") {
		t.Errorf("levels: unknown = %q, known = %q", unknown, known)
	}
}

func TestAdmittedNonOwnerSessionBelongsToThePrincipal(t *testing.T) {
	rig := newAccessRig(t, accessOpts{allowed: []string{strconv.Itoa(allowedUserID)}})
	rig.run(t, privateText(10, allowedUserID, "hello"))
	_, _, conversation := rig.onlyEnvelope(t)
	session, err := rig.h.sessions.sessions.Get(t.Context(), conversation)
	if err != nil {
		t.Fatalf("session was not ensured: %v", err)
	}
	if session.OwnerID != "telegram:555001" {
		t.Errorf("session owner = %q, want the non-owner principal", session.OwnerID)
	}
}

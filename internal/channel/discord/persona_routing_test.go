package discord

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
)

const (
	homeToken = "home-bot-token-value"
	kumaToken = "kuma-bot-token-value"
)

func twoAccountConfig() config.Discord {
	cfg := testAdapterConfig()
	home := testAdapterAccount()
	home.Name = "home"
	home.BotTokenRef = "env://AURA_TEST_HOME_TOKEN"
	home.Persona = "engineer"
	kuma := testAdapterAccount()
	kuma.Name = "kuma"
	kuma.BotTokenRef = "env://AURA_TEST_KUMA_TOKEN"
	kuma.Persona = "reviewer"
	cfg.Accounts = []config.DiscordAccount{home, kuma}
	return cfg
}

type accountAdapter struct {
	adapter  *Adapter
	sink     *memorySink
	sessions *fakeSessionEnsurer
}

func newAccountAdapter(t *testing.T, cfg *config.Discord, index int, bindings []config.Binding, stub *restStub, selfID string) *accountAdapter {
	t.Helper()
	sessions := &fakeSessionEnsurer{}
	adapter, err := New(cfg, &cfg.Accounts[index], &memoryResumeStore{}, newFakeEffectRunner(), &fakeMediaStore{}, sessions, nil)
	if err != nil {
		t.Fatalf("New(%s): %v", cfg.Accounts[index].Name, err)
	}
	adapter.SetPersonaRouter(testPersonaRouter(t, cfg, bindings))
	adapter.rateCap = 30 * time.Millisecond
	adapter.restBase = stub.srv.URL
	adapter.selfID = selfID
	sink := newMemorySink()
	adapter.sink = sink
	return &accountAdapter{adapter: adapter, sink: sink, sessions: sessions}
}

func routedEnvelope(t *testing.T, sink *memorySink, externalID string) *runtimeingress.IngressEnvelope {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	env := sink.envelopes[externalID]
	if env == nil {
		t.Fatalf("no admitted envelope for %s", externalID)
	}
	return env
}

func ensuredPersona(sessions *fakeSessionEnsurer, sessionID string) (string, bool) {
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	agent, ok := sessions.agents[sessionID]
	return agent, ok
}

func TestAccountsAdmitAndReplyThroughTheirOwnBot(t *testing.T) {
	t.Setenv("AURA_TEST_HOME_TOKEN", homeToken)
	t.Setenv("AURA_TEST_KUMA_TOKEN", kumaToken)
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m-reply")
	})
	cfg := twoAccountConfig()
	home := newAccountAdapter(t, &cfg, 0, nil, stub, "901")
	kuma := newAccountAdapter(t, &cfg, 1, nil, stub, "902")

	cases := []struct {
		account     *accountAdapter
		message     string
		source      string
		persona     string
		session     string
		wantToken   string
		otherToken  string
		channelName string
	}{
		{home, "8001", "discord:home", "engineer", "engineer/discord:home/dm:111", homeToken, kumaToken, "discord:home"},
		{kuma, "8002", "discord:kuma", "reviewer", "reviewer/discord:kuma/dm:111", kumaToken, homeToken, "discord:kuma"},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			admitted, err := tc.account.adapter.admit(t.Context(), textMessage(tc.message, "444", "", "111", "hello", nil))
			if err != nil || !admitted {
				t.Fatalf("admit() = %v, %v; want admission", admitted, err)
			}
			env := routedEnvelope(t, tc.account.sink, "message:"+tc.message)
			if env.Source != tc.source || env.AgentID != tc.persona || env.ConversationID != tc.session {
				t.Fatalf("envelope source/persona/session = %q/%q/%q, want %q/%q/%q", env.Source, env.AgentID, env.ConversationID, tc.source, tc.persona, tc.session)
			}
			if agent, ok := ensuredPersona(tc.account.sessions, tc.session); !ok || agent != tc.persona {
				t.Fatalf("ensured session persona = %q, %v; want %q", agent, ok, tc.persona)
			}
			if got := tc.account.adapter.ChannelName(); got != tc.channelName {
				t.Fatalf("ChannelName() = %q, want %q", got, tc.channelName)
			}
			stub.mu.Lock()
			before := len(stub.calls)
			stub.mu.Unlock()
			if _, err := tc.account.adapter.Deliver(t.Context(), deliveryForEnvelope(t, "turn-"+tc.message, env, "reply")); err != nil {
				t.Fatalf("Deliver(): %v", err)
			}
			stub.mu.Lock()
			defer stub.mu.Unlock()
			if len(stub.calls) <= before {
				t.Fatal("no outbound call captured")
			}
			for _, call := range stub.calls[before:] {
				if call.auth != "Bot "+tc.wantToken {
					t.Fatalf("outbound call authorized as another bot (other token used: %t)", call.auth == "Bot "+tc.otherToken)
				}
			}
		})
	}
}

func TestSameChannelServedByTwoPersonasKeepsSeparateSessions(t *testing.T) {
	t.Setenv("AURA_TEST_HOME_TOKEN", homeToken)
	t.Setenv("AURA_TEST_KUMA_TOKEN", kumaToken)
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	cfg := twoAccountConfig()
	cfg.Accounts[0].AllowedUserIDs = append(cfg.Accounts[0].AllowedUserIDs, "112")
	bindings := []config.Binding{{Persona: "researcher", Channel: config.BindingChannelDiscord, Account: "home", UserID: "112"}}
	home := newAccountAdapter(t, &cfg, 0, bindings, stub, "901")
	kuma := newAccountAdapter(t, &cfg, 1, bindings, stub, "902")

	messages := []struct {
		account *accountAdapter
		id      string
		author  string
		mention string
		want    string
	}{
		{home, "8101", "111", "901", "engineer/discord:home/channel:333"},
		{home, "8102", "112", "901", "researcher/discord:home/channel:333"},
		{kuma, "8103", "111", "902", "reviewer/discord:kuma/channel:333"},
	}
	seen := map[string]string{}
	for _, msg := range messages {
		admitted, err := msg.account.adapter.admit(t.Context(), textMessage(msg.id, "333", "222", msg.author, "hello", []string{msg.mention}))
		if err != nil || !admitted {
			t.Fatalf("admit(%s) = %v, %v; want admission", msg.id, admitted, err)
		}
		env := routedEnvelope(t, msg.account.sink, "message:"+msg.id)
		if env.ConversationID != msg.want {
			t.Fatalf("message %s session = %q, want %q", msg.id, env.ConversationID, msg.want)
		}
		if previous, dup := seen[env.ConversationID]; dup {
			t.Fatalf("messages %s and %s share session %q", previous, msg.id, env.ConversationID)
		}
		seen[env.ConversationID] = msg.id
	}
	if _, shared := ensuredPersona(kuma.sessions, "engineer/discord:home/channel:333"); shared {
		t.Fatal("kuma account ensured a session owned by the home account")
	}
}

func TestNoBindingAndNoAccountDefaultAdmitsNothing(t *testing.T) {
	t.Setenv("AURA_TEST_HOME_TOKEN", homeToken)
	t.Setenv("AURA_TEST_KUMA_TOKEN", kumaToken)
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected outbound call %s %s", r.Method, r.URL.Path)
	})
	cfg := twoAccountConfig()
	cfg.Accounts[1].Persona = ""
	kuma := newAccountAdapter(t, &cfg, 1, nil, stub, "902")

	admitted, err := kuma.adapter.admit(t.Context(), textMessage("8201", "333", "222", "111", "hello", []string{"902"}))
	if err != nil || admitted {
		t.Fatalf("admit() = %v, %v; want silent rejection", admitted, err)
	}
	if calls, _ := kuma.sink.stats("message:8201"); calls != 0 {
		t.Fatalf("sink calls = %d, want 0", calls)
	}
	kuma.sessions.mu.Lock()
	defer kuma.sessions.mu.Unlock()
	if len(kuma.sessions.sessions) != 0 {
		t.Fatalf("sessions ensured without a persona: %v", kuma.sessions.sessions)
	}
}

func TestAdapterWithoutRouterAdmitsNothing(t *testing.T) {
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	cfg := testAdapterConfig()
	sessions := &fakeSessionEnsurer{}
	adapter, err := New(&cfg, &cfg.Accounts[0], &memoryResumeStore{}, newFakeEffectRunner(), &fakeMediaStore{}, sessions, nil)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	adapter.selfID = "999"
	adapter.sink = newMemorySink()
	admitted, err := adapter.admit(t.Context(), textMessage("8301", "444", "", "111", "hello", nil))
	if err != nil || admitted {
		t.Fatalf("admit() = %v, %v; want fail-closed rejection", admitted, err)
	}
}

func TestThreadKeepsItsPersonaAfterBindingsChange(t *testing.T) {
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/channels/777300" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"777300","parent_id":"333","owner_id":"999","type":11}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	})
	cfg := testAdapterConfig()
	bindings := []config.Binding{{Persona: "reviewer", Channel: config.BindingChannelDiscord, Account: "owner", ChannelID: "333"}}
	restarted := newAccountAdapter(t, &cfg, 0, bindings, stub, "999")
	if err := restarted.sessions.EnsureSession(t.Context(), "engineer/discord:owner/thread:777300", "discord:111", "engineer"); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	for _, id := range []string{"8401", "8402"} {
		admitted, err := restarted.adapter.admit(t.Context(), textMessage(id, "777300", "222", "111", "follow-up", nil))
		if err != nil || !admitted {
			t.Fatalf("admit(%s) = %v, %v; want admission into the existing thread", id, admitted, err)
		}
		env := routedEnvelope(t, restarted.sink, "message:"+id)
		if env.AgentID != "engineer" || env.ConversationID != "engineer/discord:owner/thread:777300" {
			t.Fatalf("thread message %s routed to %q/%q, want the persona that owns the thread", id, env.AgentID, env.ConversationID)
		}
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	lookups := 0
	for _, call := range stub.calls {
		if call.method == http.MethodGet && strings.HasPrefix(call.path, "/channels/777300") {
			lookups++
		}
	}
	if lookups != 1 {
		t.Fatalf("thread lookups = %d, want one (the resolved persona is remembered)", lookups)
	}
}

func TestUnownedThreadFollowsBindingsWhenNoSessionExists(t *testing.T) {
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/channels/777301" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"777301","parent_id":"333","owner_id":"999","type":11}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	})
	cfg := testAdapterConfig()
	bindings := []config.Binding{{Persona: "reviewer", Channel: config.BindingChannelDiscord, Account: "owner", ThreadID: "777301"}}
	adapter := newAccountAdapter(t, &cfg, 0, bindings, stub, "999")

	admitted, err := adapter.adapter.admit(t.Context(), textMessage("8501", "777301", "222", "111", "hello thread", nil))
	if err != nil || !admitted {
		t.Fatalf("admit() = %v, %v; want admission", admitted, err)
	}
	env := routedEnvelope(t, adapter.sink, "message:8501")
	if env.AgentID != "reviewer" || env.ConversationID != "reviewer/discord:owner/thread:777301" {
		t.Fatalf("thread routed to %q/%q, want the thread binding", env.AgentID, env.ConversationID)
	}
}

func TestMentionInKnownThreadKeepsThreadPersona(t *testing.T) {
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/channels/777400" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"777400","parent_id":"333","owner_id":"999","type":11}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	})
	cfg := testAdapterConfig()
	cfg.Accounts[0].AllowedChannelIDs = nil
	adapter := newAccountAdapter(t, &cfg, 0, nil, stub, "999")
	if err := adapter.sessions.EnsureSession(t.Context(), "engineer/discord:owner/thread:777400", "discord:111", "engineer"); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	admitted, err := adapter.adapter.admit(t.Context(), textMessage("8801", "777400", "222", "111", "hello thread", []string{"999"}))
	if err != nil || !admitted {
		t.Fatalf("admit() = %v, %v; want admission", admitted, err)
	}
	env := routedEnvelope(t, adapter.sink, "message:8801")
	if env.AgentID != "engineer" || env.ConversationID != "engineer/discord:owner/thread:777400" {
		t.Fatalf("thread mention routed to %q/%q, want the thread persona", env.AgentID, env.ConversationID)
	}
}

func TestMentionInUnownedThreadFollowsParentBinding(t *testing.T) {
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/channels/777401" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"777401","parent_id":"333","owner_id":"999","type":11}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	})
	cfg := testAdapterConfig()
	cfg.Accounts[0].AllowedChannelIDs = nil
	bindings := []config.Binding{{Persona: "reviewer", Channel: config.BindingChannelDiscord, Account: "owner", ChannelID: "333"}}
	adapter := newAccountAdapter(t, &cfg, 0, bindings, stub, "999")

	threadType := channelTypePublicThread
	msg := textMessage("8802", "777401", "222", "111", "hello thread", []string{"999"})
	msg.ChannelType = &threadType

	admitted, err := adapter.adapter.admit(t.Context(), msg)
	if err != nil || !admitted {
		t.Fatalf("admit() = %v, %v; want admission", admitted, err)
	}
	env := routedEnvelope(t, adapter.sink, "message:8802")
	if env.AgentID != "reviewer" || env.ConversationID != "reviewer/discord:owner/thread:777401" {
		t.Fatalf("thread mention routed to %q/%q, want the parent-channel binding", env.AgentID, env.ConversationID)
	}
}

func TestTriggerThreadInheritsTriggerPersona(t *testing.T) {
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m-any")
	})
	cfg := testAdapterConfig()
	bindings := []config.Binding{{Persona: "reviewer", Channel: config.BindingChannelDiscord, Account: "owner", ChannelID: "333"}}
	adapter := newAccountAdapter(t, &cfg, 0, bindings, stub, "999")
	adapter.adapter.rememberThread("", "777302", threadRoute{parent: "333", persona: "engineer"})

	admitted, err := adapter.adapter.admit(t.Context(), textMessage("8601", "777302", "222", "111", "inside", nil))
	if err != nil || !admitted {
		t.Fatalf("admit() = %v, %v; want admission", admitted, err)
	}
	env := routedEnvelope(t, adapter.sink, "message:8601")
	if env.AgentID != "engineer" {
		t.Fatalf("thread persona = %q, want the persona recorded at thread creation", env.AgentID)
	}
}

func TestApprovalTargetBelongsToTheAdmittingAccount(t *testing.T) {
	t.Setenv("AURA_TEST_HOME_TOKEN", homeToken)
	t.Setenv("AURA_TEST_KUMA_TOKEN", kumaToken)
	stub := newRestStub(t, func(call int, w http.ResponseWriter, r *http.Request) {
		writeMessage(t, w, "m-any")
	})
	cfg := twoAccountConfig()
	home := newAccountAdapter(t, &cfg, 0, nil, stub, "901")
	kuma := newAccountAdapter(t, &cfg, 1, nil, stub, "902")
	if _, err := home.adapter.admit(t.Context(), textMessage("8701", "444", "", "111", "hello", nil)); err != nil {
		t.Fatalf("admit(): %v", err)
	}
	session := routedEnvelope(t, home.sink, "message:8701").ConversationID
	if !home.adapter.HoldsApprovalTarget(session) {
		t.Fatal("home account does not hold the approval target of its own session")
	}
	if kuma.adapter.HoldsApprovalTarget(session) {
		t.Fatal("kuma account holds an approval target for a home session")
	}
}

package discord

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func newAdmitAdapter(t *testing.T, sink *memorySink, sessions SessionEnsurer) *Adapter {
	t.Helper()
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	cfg := testAdapterConfig()
	adapter, err := newRoutedTestAdapter(t, &cfg, &memoryResumeStore{}, newFakeEffectRunner(), &fakeMediaStore{}, sessions, nil)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	adapter.selfID = "999"
	adapter.sink = sink
	return adapter
}

func textMessage(id, channel, guild, author, content string, mentions []string) *messagePayload {
	users := make([]userPayload, 0, len(mentions))
	for _, mention := range mentions {
		users = append(users, userPayload{ID: mention})
	}
	return &messagePayload{
		ID:        id,
		ChannelID: channel,
		GuildID:   guild,
		Author:    userPayload{ID: author},
		Content:   content,
		Mentions:  users,
	}
}

func ensuredOwner(sessions *fakeSessionEnsurer, sessionID string) (string, bool) {
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	owner, ok := sessions.sessions[sessionID]
	return owner, ok
}

func TestAdapter_AdmitTextEnsuresSessionOnFreshConversation(t *testing.T) {
	sessions := &fakeSessionEnsurer{}
	sink := newMemorySink()
	adapter := newAdmitAdapter(t, sink, sessions)

	admitted, err := adapter.admit(t.Context(), textMessage("9001", "333", "222", "111", "hello aura", []string{"999"}))
	if err != nil {
		t.Fatalf("admit(): %v", err)
	}
	if !admitted {
		t.Fatal("admit() = false, want admission for allowlisted text")
	}
	owner, ok := ensuredOwner(sessions, testSessionKey("channel:333"))
	if !ok {
		t.Fatal("no session row for channel:333 after text intake")
	}
	if owner != "discord:111" {
		t.Errorf("session owner = %q, want author discord:111", owner)
	}
	if calls, _ := sink.stats("message:9001"); calls != 1 {
		t.Errorf("sink calls = %d, want 1", calls)
	}
	sink.mu.Lock()
	env := sink.envelopes["message:9001"]
	sink.mu.Unlock()
	if env == nil {
		t.Fatal("no admitted envelope for message:9001")
	}
	if env.ConversationID != testSessionKey("channel:333") || env.PrincipalID != "discord:111" {
		t.Errorf("envelope = %+v, want conversation channel:333 principal discord:111", env)
	}
}

func TestAdapter_AdmitDMEnsuresSessionOnFirstContact(t *testing.T) {
	sessions := &fakeSessionEnsurer{}
	sink := newMemorySink()
	adapter := newAdmitAdapter(t, sink, sessions)

	admitted, err := adapter.admit(t.Context(), textMessage("9005", "dm-channel", "", "111", "hi aura", nil))
	if err != nil {
		t.Fatalf("admit(): %v", err)
	}
	if !admitted {
		t.Fatal("admit() = false, want admission for allowlisted DM")
	}
	owner, ok := ensuredOwner(sessions, testSessionKey("dm:111"))
	if !ok {
		t.Fatal("no session row for dm:111 after DM intake")
	}
	if owner != "discord:111" {
		t.Errorf("session owner = %q, want author discord:111", owner)
	}
}

func TestAdapter_AdmitTextExistingSessionKeepsOwner(t *testing.T) {
	sessions := &fakeSessionEnsurer{sessions: map[string]string{testSessionKey("channel:333"): "111"}}
	sink := newMemorySink()
	adapter := newAdmitAdapter(t, sink, sessions)

	admitted, err := adapter.admit(t.Context(), textMessage("9002", "333", "222", "111", "again", []string{"999"}))
	if err != nil {
		t.Fatalf("admit(): %v", err)
	}
	if !admitted {
		t.Fatal("admit() = false, want admission on existing session")
	}
	owner, ok := ensuredOwner(sessions, testSessionKey("channel:333"))
	if !ok || owner != "111" {
		t.Errorf("session owner = %q, ok = %v; want owner 111 untouched", owner, ok)
	}
	if calls, _ := sink.stats("message:9002"); calls != 1 {
		t.Errorf("sink calls = %d, want 1", calls)
	}
}

func TestAdapter_AdmitRejectionsCreateNoSession(t *testing.T) {
	botAuthor := userPayload{ID: "111", Bot: true}
	cases := map[string]*messagePayload{
		"non-allowlisted user":    textMessage("9101", "333", "222", "777", "hello aura", []string{"999"}),
		"non-allowlisted channel": textMessage("9102", "444", "222", "111", "hello aura", []string{"999"}),
		"non-allowlisted guild":   textMessage("9103", "333", "555", "111", "hello aura", []string{"999"}),
		"missing mention":         textMessage("9104", "333", "222", "111", "hello aura", nil),
		"self message":            textMessage("9105", "333", "222", "999", "hello aura", []string{"999"}),
		"empty content":           textMessage("9106", "333", "222", "111", "", []string{"999"}),
	}
	cases["bot author"] = &messagePayload{
		ID: "9107", ChannelID: "333", GuildID: "222",
		Author: botAuthor, Content: "hello aura", Mentions: []userPayload{{ID: "999"}},
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			sessions := &fakeSessionEnsurer{}
			sink := newMemorySink()
			adapter := newAdmitAdapter(t, sink, sessions)

			admitted, err := adapter.admit(t.Context(), msg)
			if err != nil {
				t.Fatalf("admit(): %v", err)
			}
			if admitted {
				t.Error("admit() = true, want rejection")
			}
			sessions.mu.Lock()
			count := len(sessions.sessions)
			sessions.mu.Unlock()
			if count != 0 {
				t.Errorf("session rows = %d, want none for rejected traffic", count)
			}
			if calls, _ := sink.stats("message:" + msg.ID); calls != 0 {
				t.Errorf("sink calls = %d, want none for rejected traffic", calls)
			}
		})
	}
}

func TestAdapter_AdmitAttachmentEnsuresSessionWithArtifacts(t *testing.T) {
	media := newMediaStub(t, "fake-png-bytes", "image/png", http.StatusOK)
	sessions := &fakeSessionEnsurer{}
	sink := newMemorySink()
	adapter := newAdmitAdapter(t, sink, sessions)

	msg := messageWithAttachments("9004", "look", []attachmentPayload{{
		URL: media.URL + "/cat.png", Filename: "cat.png", Size: 15, ContentType: "image/png",
	}})
	admitted, err := adapter.admit(t.Context(), msg)
	if err != nil {
		t.Fatalf("admit(): %v", err)
	}
	if !admitted {
		t.Fatal("admit() = false, want admission for attachment message")
	}
	owner, ok := ensuredOwner(sessions, testSessionKey("channel:333"))
	if !ok || owner != "discord:111" {
		t.Errorf("session owner = %q, ok = %v; want owner discord:111", owner, ok)
	}
	sink.mu.Lock()
	env := sink.envelopes["message:9004"]
	sink.mu.Unlock()
	if env == nil {
		t.Fatal("no admitted envelope for message:9004")
	}
	var reference replyReference
	if err := json.Unmarshal(env.ReplyContext, &reference); err != nil {
		t.Fatalf("reply context: %v", err)
	}
	if len(reference.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v, want 1", reference.Artifacts)
	}
}

func TestAdapter_AdmitDuplicateReplaysSingleTurn(t *testing.T) {
	sessions := &fakeSessionEnsurer{}
	sink := newMemorySink()
	adapter := newAdmitAdapter(t, sink, sessions)

	msg := func() *messagePayload {
		return textMessage("9003", "333", "222", "111", "hello aura", []string{"999"})
	}
	for range 2 {
		admitted, err := adapter.admit(t.Context(), msg())
		if err != nil {
			t.Fatalf("admit(): %v", err)
		}
		if !admitted {
			t.Fatal("admit() = false, want duplicate delivery to reach the sink")
		}
	}
	if calls, turns := sink.stats("message:9003"); calls != 2 || turns != 1 {
		t.Errorf("calls = %d, turns = %d; want 2 calls and 1 turn", calls, turns)
	}
	if _, ok := ensuredOwner(sessions, testSessionKey("channel:333")); !ok {
		t.Error("no session row after duplicate delivery")
	}
}

type errSessionEnsurer struct {
	err error
}

func (s *errSessionEnsurer) EnsureSession(_ context.Context, _, _, _ string) error {
	return s.err
}

func (s *errSessionEnsurer) SessionExists(_ context.Context, _ string) (bool, error) {
	return false, s.err
}

func TestAdapter_AdmitEnsureFailureSurfacesError(t *testing.T) {
	sentinel := errors.New("session store offline")
	sink := newMemorySink()
	adapter := newAdmitAdapter(t, sink, &errSessionEnsurer{err: sentinel})

	admitted, err := adapter.admit(t.Context(), textMessage("9006", "333", "222", "111", "hello aura", []string{"999"}))
	if err == nil {
		t.Fatal("admit() error = nil, want session failure")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("admit() error = %v, want the session cause preserved", err)
	}
	if admitted {
		t.Error("admit() = true, want no admission on session failure")
	}
	if calls, _ := sink.stats("message:9006"); calls != 0 {
		t.Errorf("sink calls = %d, want none on session failure", calls)
	}
}

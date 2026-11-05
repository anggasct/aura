package cli

import (
	"bytes"
	"context"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/email"
	"github.com/anggasct/aura/internal/principal"
	runtimeadk "github.com/anggasct/aura/internal/runtime/adk"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	runtimeengine "github.com/anggasct/aura/internal/runtime/engine"
	"github.com/anggasct/aura/internal/store"
)

const (
	emailTestWait    = 30 * time.Second
	emailOwnerSender = "owner@example.com"
	emailFriend      = "friend@example.com"
)

func writeEmailRuntimeConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	content := `version: 1
storage:
  path: ` + root + `
tools:
  workspace: ` + root + `
owner:
  identities:
    - email:` + emailOwnerSender + `
channels:
  email:
    enabled: true
    accounts:
      - name: work
        persona: desk
        address: aura@example.com
        authserv_id: mx.example.com
        allowed_senders:
          - ` + emailFriend + `
        imap:
          host: imap.example.com
          username: aura@example.com
          password_ref: env://AURA_EMAIL_TEST_PASSWORD
        smtp:
          host: smtp.example.com
          username: aura@example.com
          password_ref: env://AURA_EMAIL_TEST_SMTP_PASSWORD
access:
  budgets:
    trusted:
      max_tokens_per_day: 100000
      max_turns_per_minute: 50
agents:
  definitions:
    - id: desk
      description: mail desk
      instructions: "You answer mail."
      audience: trusted
      tools: [read_file]
      principal_tools: [read_file]
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	seedConfiguredStorage(t, path)
	return path
}

type countingModel struct {
	mu       sync.Mutex
	requests []*adkmodel.LLMRequest
	turns    chan struct{}
	inner    personaEvalModel
}

func (m *countingModel) Name() string { return "email-test" }

func (m *countingModel) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	select {
	case m.turns <- struct{}{}:
	default:
	}
	return m.inner.GenerateContent(ctx, req, stream)
}

type fakeMail struct {
	mu      sync.Mutex
	raw     map[uint32][]byte
	next    uint32
	waiting chan struct{}
	wake    chan struct{}
}

func newFakeMail() *fakeMail {
	return &fakeMail{raw: map[uint32][]byte{}, next: 1, waiting: make(chan struct{}, 1), wake: make(chan struct{}, 1)}
}

func (m *fakeMail) deliver(raw string) {
	m.mu.Lock()
	m.raw[m.next] = []byte(raw)
	m.next++
	m.mu.Unlock()
}

func (m *fakeMail) Connect(context.Context) (email.Session, error) {
	return &fakeMailSession{mail: m}, nil
}

type fakeMailSession struct {
	mail *fakeMail
}

func (s *fakeMailSession) Select(context.Context, string) (email.MailboxStatus, error) {
	s.mail.mu.Lock()
	defer s.mail.mu.Unlock()
	return email.MailboxStatus{UIDValidity: 7, UIDNext: s.mail.next}, nil
}

func (s *fakeMailSession) List(_ context.Context, first, last uint32) ([]email.MessageInfo, error) {
	s.mail.mu.Lock()
	defer s.mail.mu.Unlock()
	var infos []email.MessageInfo
	for uid := first; uid <= last; uid++ {
		if raw, ok := s.mail.raw[uid]; ok {
			infos = append(infos, email.MessageInfo{UID: uid, Size: int64(len(raw))})
		}
	}
	return infos, nil
}

func (s *fakeMailSession) Header(_ context.Context, uid uint32, limit int64) ([]byte, error) {
	s.mail.mu.Lock()
	defer s.mail.mu.Unlock()
	raw := s.mail.raw[uid]
	if end := bytes.Index(raw, []byte("\r\n\r\n")); end >= 0 {
		raw = raw[:end+4]
	}
	if int64(len(raw)) > limit+1 {
		raw = raw[:limit+1]
	}
	return raw, nil
}

func (s *fakeMailSession) Message(_ context.Context, uid uint32, _ int64) ([]byte, error) {
	s.mail.mu.Lock()
	defer s.mail.mu.Unlock()
	return s.mail.raw[uid], nil
}

func (s *fakeMailSession) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case s.mail.waiting <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.mail.wake:
		return nil
	}
}

func (s *fakeMailSession) Close() error { return nil }

func emailMessage(from, messageID, subject, body string) string {
	return strings.Join([]string{
		"Authentication-Results: mx.example.com; dmarc=pass header.from=example.com",
		"From: " + from,
		"To: aura@example.com",
		"Subject: " + subject,
		"Message-ID: <" + messageID + ">",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
		"",
	}, "\r\n")
}

func TestEmailTurnsRunThroughTheRealRuntime(t *testing.T) {
	path := writeEmailRuntimeConfig(t)
	cfg := mustLoadConfig(t, path)
	db := openAccessDB(t, path)
	registry, err := buildAgentRegistry(cfg)
	if err != nil {
		t.Fatalf("buildAgentRegistry: %v", err)
	}
	passEvals(t, db, registry, "desk")
	model := &countingModel{turns: make(chan struct{}, 16)}
	name := "email-model-" + strings.ReplaceAll(t.Name(), "/", "-")
	adkmodel.Register("^"+name+"$", func(context.Context, string) (adkmodel.LLM, error) { return model, nil })
	events := store.NewEventStore(db)
	executor, err := runtimeadk.NewADKExecutor("aura", name, store.NewSessionService(db), events, personaEvalDenyAll{}, nil, nil,
		runtimeadk.WithBuiltinToolExecutor(catalogExecutor{}),
		runtimeadk.WithAgentResolver(registry, nil),
	)
	if err != nil {
		t.Fatalf("NewADKExecutor: %v", err)
	}
	engine, err := runtimeengine.NewEngine(&runtimeengine.Config{
		DefaultAgentID: auraagent.DefaultID,
		Personas:       registryPersonas{registry: registry},
	}, events, store.NewDedupeStore(db), executor, nil)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	router, err := auraagent.NewRouter(registry, &cfg.Channels, cfg.Bindings)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	owners, err := ownerResolver(cfg)
	if err != nil {
		t.Fatalf("ownerResolver: %v", err)
	}
	mail := newFakeMail()
	adapter, err := email.New(&cfg.Channels.Email, &cfg.Channels.Email.Accounts[0], mail,
		emailCursors{cursors: store.NewEmailCursorStore(db)},
		&emailSessions{discordSessionEnsurer{sessions: store.NewSessionService(db)}},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	adapter.SetPrincipalResolver(owners)
	adapter.SetPersonaRouter(router)
	settings := discordAccessSettings(cfg, db, nil)
	adapter.SetAccess(&email.AccessSettings{Budgets: settings.Budgets, Evals: settings.Evals})
	host, err := runtimeengine.NewHost(engine, []runtimechannelhost.ChannelPort{adapter}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	if err := host.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), emailTestWait)
		defer cancel()
		if err := host.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	awaitSignal(t, mail.waiting, "adapter never waited for mail")

	mail.deliver(emailMessage(emailOwnerSender, "owner-1@example.com", "owner question", "what is on my calendar?"))
	mail.deliver(emailMessage(emailOwnerSender, "owner-1@example.com", "owner question", "what is on my calendar?"))
	mail.deliver(emailMessage(emailFriend, "friend-1@example.com", "friend question", "can you send the notes?"))
	mail.wake <- struct{}{}

	awaitSignal(t, model.turns, "first turn never reached the model")
	awaitSignal(t, model.turns, "second turn never reached the model")

	model.mu.Lock()
	requests := append([]*adkmodel.LLMRequest(nil), model.requests...)
	model.mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("model requests = %d, want 2 (a repeated Message-ID must not run a second turn)", len(requests))
	}
	if text := contentText(requests[0]); !strings.Contains(text, "what is on my calendar?") || !strings.Contains(text, "Subject: owner question") {
		t.Errorf("owner turn input = %q", text)
	}
	if text := contentText(requests[1]); !strings.Contains(text, "can you send the notes?") {
		t.Errorf("trusted turn input = %q", text)
	}

	owned := map[string]string{}
	rows, err := db.QueryContext(t.Context(), `SELECT id, owner_id FROM session WHERE id LIKE 'desk/email:work/%'`)
	if err != nil {
		t.Fatalf("query sessions: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, owner string
		if err := rows.Scan(&id, &owner); err != nil {
			t.Fatalf("scan: %v", err)
		}
		owned[id] = owner
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(owned) != 2 {
		t.Fatalf("email sessions = %v, want 2", owned)
	}
	counts := map[string]int{}
	for _, owner := range owned {
		counts[owner]++
	}
	if counts[principal.OwnerID] != 1 || counts["email:"+emailFriend] != 1 {
		t.Fatalf("session owners = %v", owned)
	}
	for id := range owned {
		if !strings.Contains(id, "/thread:") {
			t.Errorf("session id %q lacks the thread segment", id)
		}
	}

	_, err = adapter.Deliver(t.Context(), &runtimechannelhost.DeliveryRequest{})
	if code, ok := email.CodeOf(err); !ok || code != email.ErrorCodeDeliveryUnavailable {
		t.Fatalf("Deliver error = %v, want %s", err, email.ErrorCodeDeliveryUnavailable)
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(emailTestWait):
		t.Fatal(failure)
	}
}

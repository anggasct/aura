package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"log/slog"
	netmail "net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/email"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/principal"
	runtimeadk "github.com/anggasct/aura/internal/runtime/adk"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	runtimeengine "github.com/anggasct/aura/internal/runtime/engine"
	"github.com/anggasct/aura/internal/store"
	toolsbuiltin "github.com/anggasct/aura/internal/tools/builtin"
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

func emailMessage(from, messageID, subject, body string, extra ...string) string {
	lines := []string{
		"Authentication-Results: mx.example.com; dmarc=pass header.from=example.com",
		"From: " + from,
		"To: aura@example.com",
		"Subject: " + subject,
		"Message-ID: <" + messageID + ">",
	}
	lines = append(lines, extra...)
	return strings.Join(append(lines, "Content-Type: text/plain; charset=utf-8", "", body, ""), "\r\n")
}

type fakeSubmissions struct {
	mu        sync.Mutex
	sent      []email.Submission
	submitted chan struct{}
}

func newFakeSubmissions() *fakeSubmissions {
	return &fakeSubmissions{submitted: make(chan struct{}, 16)}
}

func (m *fakeSubmissions) Submit(_ context.Context, submission *email.Submission) error {
	m.mu.Lock()
	m.sent = append(m.sent, *submission)
	m.mu.Unlock()
	m.submitted <- struct{}{}
	return nil
}

func (m *fakeSubmissions) all() []email.Submission {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]email.Submission(nil), m.sent...)
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
	name := fmt.Sprintf("email-model-%d", personaEvalModelSeq.Add(1))
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
	submissions := newFakeSubmissions()
	effects, err := toolsbuiltin.NewChannelEffects(db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewChannelEffects: %v", err)
	}
	adapter, err := email.New(&cfg.Channels.Email, &cfg.Channels.Email.Accounts[0], mail, submissions,
		emailCursors{cursors: store.NewEmailCursorStore(db)}, effects,
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
	shutdown := sync.OnceFunc(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), emailTestWait)
		defer cancel()
		if err := host.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	t.Cleanup(shutdown)
	awaitSignal(t, mail.waiting, "adapter never waited for mail")

	ownerMail := emailMessage(emailOwnerSender, "owner-1@example.com", "owner question", "what is on my calendar?",
		"Reply-To: attacker@evil.example", "Cc: bystander@example.org", "References: <root-1@example.com>")
	mail.deliver(ownerMail)
	mail.deliver(ownerMail)
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
	var ownerInputs, friendInputs int
	for _, request := range requests {
		text := contentText(request)
		switch {
		case strings.Contains(text, "what is on my calendar?") && strings.Contains(text, "Subject: owner question"):
			ownerInputs++
		case strings.Contains(text, "can you send the notes?") && strings.Contains(text, "Subject: friend question"):
			friendInputs++
		default:
			t.Errorf("unexpected turn input = %q", text)
		}
	}
	if ownerInputs != 1 || friendInputs != 1 {
		t.Errorf("owner turns = %d, trusted turns = %d, want one of each", ownerInputs, friendInputs)
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

	awaitSignal(t, submissions.submitted, "first reply never reached the submission server")
	awaitSignal(t, submissions.submitted, "second reply never reached the submission server")

	sent := submissions.all()
	if len(sent) != 2 {
		t.Fatalf("submissions = %d, want one reply per turn", len(sent))
	}
	byRecipient := map[string]email.Submission{}
	for _, submission := range sent {
		byRecipient[submission.To] = submission
	}
	for _, recipient := range []string{emailOwnerSender, emailFriend} {
		submission, ok := byRecipient[recipient]
		if !ok {
			t.Fatalf("no reply addressed to the authenticated sender %s; got %d recipients", recipient, len(byRecipient))
		}
		if submission.From != "aura@example.com" {
			t.Errorf("reply from %q, want the account address", submission.From)
		}
		parsed, err := netmail.ReadMessage(bytes.NewReader(submission.Message))
		if err != nil {
			t.Fatalf("ReadMessage: %v", err)
		}
		if parsed.Header.Get("To") != recipient || parsed.Header.Get("Auto-Submitted") != "auto-replied" {
			t.Errorf("reply headers To=%q Auto-Submitted=%q", parsed.Header.Get("To"), parsed.Header.Get("Auto-Submitted"))
		}
		for _, header := range []string{"Reply-To", "Cc", "Bcc"} {
			if _, present := parsed.Header[header]; present {
				t.Errorf("reply to %s carries a %s header", recipient, header)
			}
		}
		if body, _ := io.ReadAll(parsed.Body); strings.TrimSpace(string(body)) != "ok" {
			t.Errorf("reply body = %q, want the model answer", body)
		}
	}
	ownerReply, err := netmail.ReadMessage(bytes.NewReader(byRecipient[emailOwnerSender].Message))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if ownerReply.Header.Get("Subject") != "Re: owner question" || ownerReply.Header.Get("In-Reply-To") != "<owner-1@example.com>" ||
		strings.Join(strings.Fields(ownerReply.Header.Get("References")), " ") != "<root-1@example.com> <owner-1@example.com>" {
		t.Errorf("owner reply threading: Subject=%q In-Reply-To=%q References=%q", ownerReply.Header.Get("Subject"), ownerReply.Header.Get("In-Reply-To"), ownerReply.Header.Get("References"))
	}
	shutdown()
	var intents int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM effect_intent WHERE provider = 'email' AND state = 'succeeded'`).Scan(&intents); err != nil || intents != 2 {
		t.Errorf("succeeded email intents = %d (%v), want 2", intents, err)
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

func TestBuildSecretVaultRedactsEmailPasswords(t *testing.T) {
	t.Setenv("AURA_TEST_VAULT_IMAP", "imap-secret-probe")
	t.Setenv("AURA_TEST_VAULT_SMTP", "smtp-secret-probe")
	cfg := config.Default()
	cfg.Channels.Email.Accounts = []config.EmailAccount{{
		Name: "work",
		IMAP: config.EmailIMAP{PasswordRef: "env://AURA_TEST_VAULT_IMAP"},
		SMTP: config.EmailSMTP{PasswordRef: "env://AURA_TEST_VAULT_SMTP"},
	}}
	vault := buildSecretVault(&cfg)
	for _, secret := range []string{"imap-secret-probe", "smtp-secret-probe"} {
		if got := vault.Redact("password " + secret); strings.Contains(got, secret) {
			t.Errorf("Redact() = %q, want %s hidden", got, secret)
		}
	}
}

package email

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
	"github.com/anggasct/aura/internal/channel/access"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/principal"
	runtimeingress "github.com/anggasct/aura/internal/runtime/ingress"
	"github.com/anggasct/aura/internal/store/storetest"
)

const (
	testAccount   = "work"
	testPersona   = "main"
	testAddress   = "aura@example.com"
	testAuthserv  = "mx.example.com"
	ownerAddress  = "owner@example.com"
	friendAddress = "friend@example.com"
	strangerAddr  = "stranger@example.net"
	waitDeadline  = 30 * time.Second
)

type mailSpec struct {
	from       string
	subject    string
	messageID  string
	references string
	inReplyTo  string
	body       string
	extra      []string
	auth       string
	noAuth     bool
	noID       bool
	rawFrom    []string
	below      []string
	extraID    string
}

func (m mailSpec) raw() []byte {
	from := m.from
	if from == "" {
		from = ownerAddress
	}
	var out strings.Builder
	for _, line := range m.extra {
		out.WriteString(line + "\r\n")
	}
	if !m.noAuth {
		auth := m.auth
		if auth == "" {
			auth = testAuthserv + "; dmarc=pass header.from=" + domainOf(from)
		}
		out.WriteString("Authentication-Results: " + auth + "\r\n")
	}
	for _, line := range m.below {
		out.WriteString(line + "\r\n")
	}
	if m.rawFrom != nil {
		for _, line := range m.rawFrom {
			out.WriteString(line + "\r\n")
		}
	} else {
		out.WriteString("From: Someone <" + from + ">\r\n")
	}
	out.WriteString("To: " + testAddress + "\r\n")
	out.WriteString("Subject: " + m.subject + "\r\n")
	out.WriteString("Date: Thu, 05 Nov 2026 09:00:00 +0000\r\n")
	if !m.noID {
		id := m.messageID
		if id == "" {
			id = "default@example.com"
		}
		out.WriteString("Message-ID: <" + id + ">\r\n")
		if m.extraID != "" {
			out.WriteString("Message-ID: <" + m.extraID + ">\r\n")
		}
	}
	if m.references != "" {
		out.WriteString("References: " + m.references + "\r\n")
	}
	if m.inReplyTo != "" {
		out.WriteString("In-Reply-To: " + m.inReplyTo + "\r\n")
	}
	out.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	body := m.body
	if body == "" {
		body = "hello"
	}
	out.WriteString(body + "\r\n")
	return []byte(out.String())
}

type storedMail struct {
	raw   []byte
	ghost bool
}

type fakeMailbox struct {
	mu        sync.Mutex
	validity  uint32
	uidNext   uint32
	mail      map[uint32]storedMail
	sizes     map[uint32]int64
	wake      chan struct{}
	waiting   chan time.Duration
	connects  int
	connected chan struct{}
	connect   []error
	selects   int
	headers   []uint32
	bodies    []uint32
	waits     []time.Duration
	failList  error
}

func newFakeMailbox() *fakeMailbox {
	return &fakeMailbox{
		validity:  1,
		uidNext:   1,
		mail:      map[uint32]storedMail{},
		sizes:     map[uint32]int64{},
		wake:      make(chan struct{}, 8),
		waiting:   make(chan time.Duration, 64),
		connected: make(chan struct{}, 64),
	}
}

func (m *fakeMailbox) add(raw []byte) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	uid := m.uidNext
	m.uidNext++
	m.mail[uid] = storedMail{raw: raw}
	m.sizes[uid] = int64(len(raw))
	return uid
}

func (m *fakeMailbox) addGhost(raw []byte) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	uid := m.uidNext
	m.uidNext++
	m.mail[uid] = storedMail{raw: raw, ghost: true}
	m.sizes[uid] = int64(len(raw))
	return uid
}

func (m *fakeMailbox) remove(uid uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mail, uid)
	delete(m.sizes, uid)
}

func (m *fakeMailbox) setSize(uid uint32, size int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sizes[uid] = size
}

func (m *fakeMailbox) reset(validity uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validity = validity
	m.uidNext = 1
	m.mail = map[uint32]storedMail{}
	m.sizes = map[uint32]int64{}
}

func (m *fakeMailbox) Connect(context.Context) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connects++
	select {
	case m.connected <- struct{}{}:
	default:
	}
	if len(m.connect) > 0 {
		err := m.connect[0]
		m.connect = m.connect[1:]
		if err != nil {
			return nil, err
		}
	}
	return &fakeSession{mailbox: m}, nil
}

type fakeSession struct {
	mailbox *fakeMailbox
}

func (s *fakeSession) Select(context.Context, string) (MailboxStatus, error) {
	s.mailbox.mu.Lock()
	defer s.mailbox.mu.Unlock()
	s.mailbox.selects++
	return MailboxStatus{UIDValidity: s.mailbox.validity, UIDNext: s.mailbox.uidNext}, nil
}

func (s *fakeSession) List(_ context.Context, first, last uint32) ([]MessageInfo, error) {
	s.mailbox.mu.Lock()
	defer s.mailbox.mu.Unlock()
	if s.mailbox.failList != nil {
		return nil, s.mailbox.failList
	}
	var infos []MessageInfo
	for uid := range s.mailbox.mail {
		if uid >= first && uid <= last {
			infos = append(infos, MessageInfo{UID: uid, Size: s.mailbox.sizes[uid]})
		}
	}
	return infos, nil
}

func (s *fakeSession) Header(_ context.Context, uid uint32, limit int64) ([]byte, error) {
	s.mailbox.mu.Lock()
	defer s.mailbox.mu.Unlock()
	s.mailbox.headers = append(s.mailbox.headers, uid)
	stored, ok := s.mailbox.mail[uid]
	if !ok || stored.ghost {
		return nil, Errorf(ErrorCodeMessageUnavailable, "gone")
	}
	end := bytes.Index(stored.raw, []byte("\r\n\r\n"))
	header := stored.raw[:end+4]
	if int64(len(header)) > limit+1 {
		header = header[:limit+1]
	}
	return header, nil
}

func (s *fakeSession) Message(_ context.Context, uid uint32, limit int64) ([]byte, error) {
	s.mailbox.mu.Lock()
	defer s.mailbox.mu.Unlock()
	s.mailbox.bodies = append(s.mailbox.bodies, uid)
	stored, ok := s.mailbox.mail[uid]
	if !ok {
		return nil, Errorf(ErrorCodeMessageUnavailable, "gone")
	}
	raw := stored.raw
	if int64(len(raw)) > limit+1 {
		raw = raw[:limit+1]
	}
	return raw, nil
}

func (s *fakeSession) Wait(ctx context.Context, interval time.Duration) error {
	s.mailbox.mu.Lock()
	s.mailbox.waits = append(s.mailbox.waits, interval)
	s.mailbox.mu.Unlock()
	select {
	case s.mailbox.waiting <- interval:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.mailbox.wake:
		return nil
	}
}

func (s *fakeSession) Close() error { return nil }

type memoryCursors struct {
	mu     sync.Mutex
	values map[string]Cursor
	saves  int
}

func newMemoryCursors() *memoryCursors {
	return &memoryCursors{values: map[string]Cursor{}}
}

func (c *memoryCursors) Load(_ context.Context, source string) (Cursor, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.values[source]
	return value, ok, nil
}

func (c *memoryCursors) Save(_ context.Context, source string, cursor Cursor, _ time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saves++
	if previous, ok := c.values[source]; ok && previous.UIDValidity == cursor.UIDValidity && previous.LastUID > cursor.LastUID {
		return nil
	}
	c.values[source] = cursor
	return nil
}

func (c *memoryCursors) get(source string) Cursor {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[source]
}

type memorySessions struct {
	mu     sync.Mutex
	owners map[string]string
	agents map[string]string
}

func newMemorySessions() *memorySessions {
	return &memorySessions{owners: map[string]string{}, agents: map[string]string{}}
}

func (s *memorySessions) EnsureSession(_ context.Context, sessionID, ownerID, agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.owners[sessionID]; !ok {
		s.owners[sessionID] = ownerID
		s.agents[sessionID] = agentID
	}
	return nil
}

func (s *memorySessions) SessionOwner(_ context.Context, sessionID string) (owner string, found bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, ok := s.owners[sessionID]
	return owner, ok, nil
}

func (s *memorySessions) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.owners)
}

type captureSink struct {
	notify    chan struct{}
	mu        sync.Mutex
	envelopes []*runtimeingress.IngressEnvelope
	seen      map[string]bool
	fresh     int
	err       error
}

func newCaptureSink() *captureSink {
	return &captureSink{seen: map[string]bool{}, notify: make(chan struct{}, 64)}
}

func (s *captureSink) Accept(_ context.Context, env *runtimeingress.IngressEnvelope) (runtimeingress.TurnRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return runtimeingress.TurnRef{}, s.err
	}
	s.envelopes = append(s.envelopes, env)
	select {
	case s.notify <- struct{}{}:
	default:
	}
	replay := s.seen[env.ExternalID]
	s.seen[env.ExternalID] = true
	if !replay {
		s.fresh++
	}
	return runtimeingress.TurnRef{TurnID: "turn-" + env.ExternalID, SessionID: env.ConversationID, Replayed: replay}, nil
}

func (s *captureSink) all() []*runtimeingress.IngressEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*runtimeingress.IngressEnvelope(nil), s.envelopes...)
}

func (s *captureSink) turns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fresh
}

type fakeRouter struct {
	definition auraagent.Definition
}

func (r fakeRouter) Resolve(scope *auraagent.RouteScope) (auraagent.RouteMatch, bool) {
	if scope.Channel != config.BindingChannelEmail || scope.Account != testAccount {
		return auraagent.RouteMatch{}, false
	}
	return auraagent.RouteMatch{Persona: r.definition.ID, Score: auraagent.ScoreAccountDefault, Binding: auraagent.AccountDefaultBinding}, true
}

func (r fakeRouter) Persona(id string) (auraagent.Definition, bool) {
	if id != r.definition.ID {
		return auraagent.Definition{}, false
	}
	return r.definition, true
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fakeMailer struct {
	mu          sync.Mutex
	results     []error
	submissions []Submission
}

func (m *fakeMailer) fail(errs ...error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results = append(m.results, errs...)
}

func (m *fakeMailer) Submit(_ context.Context, submission *Submission) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.submissions = append(m.submissions, *submission)
	if len(m.results) == 0 {
		return nil
	}
	err := m.results[0]
	m.results = m.results[1:]
	return err
}

func (m *fakeMailer) sent() []Submission {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Submission(nil), m.submissions...)
}

func newEffects(t *testing.T, db *sql.DB) *effect.Executor {
	t.Helper()
	journal, err := effect.NewJournal(db, effect.Options{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	executor, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	return executor
}

type harness struct {
	t        *testing.T
	cfg      *config.Email
	account  *config.EmailAccount
	logger   *slog.Logger
	adapter  *Adapter
	mailer   Mailer
	db       *sql.DB
	mailbox  *fakeMailbox
	sink     *captureSink
	cursors  *memoryCursors
	sessions *memorySessions
	logs     *lockedBuffer
	mu       sync.Mutex
	drops    []string
	admitted []principal.Class
	cancel   context.CancelFunc
	done     chan struct{}
}

type harnessOptions struct {
	account  func(*config.EmailAccount)
	email    func(*config.Email)
	audience string
	budgets  access.BudgetGate
	evals    access.EvalGate
	owners   []string
	mailbox  *fakeMailbox
	mailer   Mailer
	db       *sql.DB
	cursors  *memoryCursors
	sessions *memorySessions
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()
	cfg := &config.Email{Enabled: true, MaxAttachmentBytes: config.EmailMaxAttachmentBytes}
	account := &config.EmailAccount{
		Name:           testAccount,
		Persona:        testPersona,
		Address:        testAddress,
		AuthservID:     testAuthserv,
		AllowedSenders: []string{friendAddress},
		IMAP: config.EmailIMAP{
			Host: "imap.example.com", Port: 993, Username: testAddress, PasswordRef: "env://AURA_TEST_EMAIL_PASSWORD",
			Folder: "INBOX", PollInterval: config.Duration(time.Minute),
		},
	}
	if opts.email != nil {
		opts.email(cfg)
	}
	if opts.account != nil {
		opts.account(account)
	}
	mailbox := opts.mailbox
	if mailbox == nil {
		mailbox = newFakeMailbox()
	}
	cursors := opts.cursors
	if cursors == nil {
		cursors = newMemoryCursors()
	}
	sessions := opts.sessions
	if sessions == nil {
		sessions = newMemorySessions()
	}
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	mailer := opts.mailer
	if mailer == nil {
		mailer = &fakeMailer{}
	}
	db := opts.db
	if db == nil {
		db = storetest.OpenMigrated(t)
	}
	adapter, err := New(cfg, account, mailbox, mailer, cursors, newEffects(t, db), sessions, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	identities := opts.owners
	if identities == nil {
		identities = []string{"email:" + ownerAddress}
	}
	owners, err := principal.NewResolver(identities)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	audience := opts.audience
	if audience == "" {
		audience = config.AudienceTrusted
	}
	adapter.SetPrincipalResolver(owners)
	adapter.SetPersonaRouter(fakeRouter{definition: auraagent.Definition{ID: testPersona, Audience: audience}})
	adapter.SetAccess(&AccessSettings{Budgets: opts.budgets, Evals: opts.evals})
	adapter.retryBase = time.Millisecond
	adapter.retryCap = 5 * time.Millisecond
	h := &harness{t: t, cfg: cfg, account: account, logger: logger, adapter: adapter, mailer: mailer, db: db, mailbox: mailbox, sink: newCaptureSink(), cursors: cursors, sessions: sessions, logs: logs, done: make(chan struct{})}
	adapter.SetDropObserver(func(_ context.Context, reason string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.drops = append(h.drops, reason)
	})
	adapter.SetAdmissionObserver(func(_ context.Context, class principal.Class) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.admitted = append(h.admitted, class)
	})
	return h
}

func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(h.t.Context())
	h.cancel = cancel
	go func() {
		defer close(h.done)
		if err := h.adapter.Start(ctx, h.sink); err != nil {
			h.t.Errorf("Start: %v", err)
		}
	}()
	h.t.Cleanup(h.stop)
	h.awaitWaiting()
	for len(h.mailbox.connected) > 0 {
		<-h.mailbox.connected
	}
}

func (h *harness) awaitReconnect() {
	h.t.Helper()
	select {
	case <-h.mailbox.connected:
	case <-time.After(waitDeadline):
		h.t.Fatal("adapter never reconnected")
	}
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(waitDeadline):
		h.t.Error("adapter did not stop")
	}
}

func (h *harness) awaitWaiting() {
	h.t.Helper()
	select {
	case <-h.mailbox.waiting:
	case <-time.After(waitDeadline):
		h.t.Fatal("adapter never reached its wait state")
	}
}

func (h *harness) deliver(raws ...[]byte) {
	h.t.Helper()
	for _, raw := range raws {
		h.mailbox.add(raw)
	}
	h.mailbox.wake <- struct{}{}
	h.awaitWaiting()
}

func (h *harness) wakeCycle() {
	h.t.Helper()
	h.mailbox.wake <- struct{}{}
	h.awaitWaiting()
}

func (h *harness) dropReasons() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.drops...)
}

func (h *harness) admittedClasses() []principal.Class {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]principal.Class(nil), h.admitted...)
}

func (h *harness) wantDrops(want ...string) {
	h.t.Helper()
	got := h.dropReasons()
	if len(got) != len(want) {
		h.t.Fatalf("drops = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			h.t.Fatalf("drops = %v, want %v", got, want)
		}
	}
}

func (h *harness) wantTurns(n int) {
	h.t.Helper()
	if got := len(h.sink.all()); got != n {
		h.t.Fatalf("envelopes = %d, want %d (drops %v)", got, n, h.dropReasons())
	}
}

type failingBudgets struct {
	refusal string
	err     error
}

func (b failingBudgets) Admit(context.Context, string, principal.Class) (string, error) {
	return b.refusal, b.err
}

type staticEvals struct {
	evaluated bool
	err       error
}

func (e staticEvals) Evaluated(context.Context, string) (bool, error) {
	return e.evaluated, e.err
}

var errBoom = errors.New("boom")

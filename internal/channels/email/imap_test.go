package email

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/anggasct/aura/internal/config"
)

const (
	imapUser         = "aura@example.com"
	imapPassword     = "correct horse"
	imapPasswordName = "AURA_TEST_EMAIL_PASSWORD"
)

type literal struct {
	*bytes.Reader
}

func (l literal) Size() int64 { return int64(l.Len()) }

type testCA struct {
	pool *x509.CertPool
	cert tls.Certificate
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "aura test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "imap.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return &testCA{pool: pool, cert: tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}
}

type imapFixture struct {
	t      *testing.T
	ca     *testCA
	user   *imapmemserver.User
	host   string
	port   int
	idling *atomic.Int64
}

func (f *imapFixture) awaitIdleEntries(want int64) {
	f.t.Helper()
	deadline := time.After(waitDeadline)
	for f.idling.Load() < want {
		select {
		case <-time.After(time.Millisecond):
		case <-deadline:
			f.t.Fatalf("idle entries = %d, want %d", f.idling.Load(), want)
		}
	}
}

type idleSession struct {
	*imapmemserver.UserSession
	user    *imapmemserver.User
	started *atomic.Int64
}

func (s *idleSession) Login(username, password string) error {
	if err := s.user.Login(username, password); err != nil {
		return err
	}
	s.UserSession = imapmemserver.NewUserSession(s.user)
	return nil
}

func (s *idleSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	s.started.Add(1)
	return s.UserSession.Idle(w, stop)
}

func startIMAPServer(t *testing.T, caps imap.CapSet) *imapFixture {
	t.Helper()
	ca := newTestCA(t)
	user := imapmemserver.NewUser(imapUser, imapPassword)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	idling := new(atomic.Int64)
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &idleSession{user: user, started: idling}, nil, nil
		},
		Caps:   caps,
		Logger: discardLogger{},
	})
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{ca.cert}, MinVersion: tls.VersionTLS12})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(tlsListener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		<-done
	})
	port := listenerPort(t, listener)
	t.Setenv(imapPasswordName, imapPassword)
	return &imapFixture{t: t, ca: ca, user: user, host: "127.0.0.1", port: port, idling: idling}
}

var (
	rev1Caps = imap.CapSet{imap.CapIMAP4rev1: {}}
	rev2Caps = imap.CapSet{imap.CapIMAP4rev2: {}}
)

type pollingConnector struct {
	inner Connector
}

func (c pollingConnector) Connect(ctx context.Context) (Session, error) {
	session, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if imapSession, ok := session.(*imapSession); ok {
		imapSession.idle = false
	}
	return session, nil
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

func (f *imapFixture) append(raw []byte) {
	f.t.Helper()
	if _, err := f.user.Append("INBOX", literal{bytes.NewReader(raw)}, &imap.AppendOptions{}); err != nil {
		f.t.Fatalf("append: %v", err)
	}
}

func (f *imapFixture) account(poll time.Duration) *config.EmailAccount {
	return &config.EmailAccount{
		Name:           testAccount,
		Persona:        testPersona,
		Address:        testAddress,
		AuthservID:     testAuthserv,
		AllowedSenders: []string{friendAddress},
		IMAP: config.EmailIMAP{
			Host: f.host, Port: f.port, Username: imapUser, PasswordRef: "env://" + imapPasswordName,
			Folder: "INBOX", PollInterval: config.Duration(poll),
		},
	}
}

func (f *imapFixture) connector(account *config.EmailAccount) *imapConnector {
	f.t.Helper()
	connector, err := newIMAPConnector(account, f.ca.pool)
	if err != nil {
		f.t.Fatalf("newIMAPConnector: %v", err)
	}
	return connector
}

func (f *imapFixture) harness(poll time.Duration, polling bool, opts harnessOptions) *harness {
	f.t.Helper()
	opts.account = func(a *config.EmailAccount) { *a = *f.account(poll) }
	h := newHarness(f.t, opts)
	var connector Connector = f.connector(h.adapter.account)
	if polling {
		connector = pollingConnector{inner: connector}
	}
	h.adapter.connector = connector
	return h
}

func (h *harness) awaitEnvelopes(n int) {
	h.t.Helper()
	deadline := time.After(waitDeadline)
	for len(h.sink.all()) < n {
		select {
		case <-h.sink.notify:
		case <-deadline:
			h.t.Fatalf("envelopes = %d, want %d (drops %v)", len(h.sink.all()), n, h.dropReasons())
		}
	}
}

func (h *harness) startAndAwaitCursor() {
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
	deadline := time.After(waitDeadline)
	for {
		if _, found, _ := h.cursors.Load(ctx, h.adapter.source()); found {
			return
		}
		select {
		case <-time.After(time.Millisecond):
		case <-deadline:
			h.t.Fatal("adapter never stored its first cursor")
		}
	}
}

func TestIMAP_IdleDeliversNewMailWithoutPolling(t *testing.T) {
	server := startIMAPServer(t, rev1Caps)
	h := server.harness(time.Hour, false, harnessOptions{})
	h.startAndAwaitCursor()
	server.awaitIdleEntries(1)
	server.append(mailSpec{subject: "over idle", messageID: "idle@example.com"}.raw())
	h.awaitEnvelopes(1)
	env := h.sink.all()[0]
	if env.PrincipalClass != "owner" || env.Parts[0].Text != "Subject: over idle\n\nhello" {
		t.Errorf("envelope = %+v", env)
	}
	server.awaitIdleEntries(2)
	server.append(mailSpec{from: friendAddress, subject: "second", messageID: "idle2@example.com"}.raw())
	h.awaitEnvelopes(2)
	if got := h.sink.all()[1].PrincipalClass; got != "trusted" {
		t.Errorf("second class = %q, want trusted", got)
	}
}

func TestIMAP_IdleOnAnIMAP4rev2OnlyServer(t *testing.T) {
	server := startIMAPServer(t, rev2Caps)
	session, err := server.connector(server.account(time.Hour)).Connect(t.Context())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = session.Close() }()
	imapped, ok := session.(*imapSession)
	if !ok {
		t.Fatalf("session type %T", session)
	}
	if !imapped.idle {
		t.Fatal("an IMAP4rev2 server is not treated as IDLE capable")
	}
}

func TestSupportsIdle(t *testing.T) {
	cases := map[string]struct {
		caps imap.CapSet
		want bool
	}{
		"idle extension": {caps: imap.CapSet{imap.CapIMAP4rev1: {}, imap.CapIdle: {}}, want: true},
		"imap4rev2":      {caps: imap.CapSet{imap.CapIMAP4rev2: {}}, want: true},
		"imap4rev1 only": {caps: imap.CapSet{imap.CapIMAP4rev1: {}}},
		"nothing":        {caps: imap.CapSet{}},
		"nil":            {},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := supportsIdle(tc.caps); got != tc.want {
				t.Fatalf("supportsIdle() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIMAP_PollingPathWhenTheServerHasNoIdle(t *testing.T) {
	server := startIMAPServer(t, rev1Caps)
	h := server.harness(5*time.Millisecond, true, harnessOptions{})
	h.startAndAwaitCursor()
	server.append(mailSpec{subject: "polled", messageID: "poll@example.com"}.raw())
	h.awaitEnvelopes(1)
	if h.sink.all()[0].ExternalID != "email:work:poll@example.com" {
		t.Errorf("ExternalID = %q", h.sink.all()[0].ExternalID)
	}
}

func TestIMAP_ChangedUIDValidityIngestsNoOldMail(t *testing.T) {
	server := startIMAPServer(t, rev1Caps)
	h := server.harness(5*time.Millisecond, true, harnessOptions{})
	h.startAndAwaitCursor()
	server.append(mailSpec{messageID: "before@example.com"}.raw())
	h.awaitEnvelopes(1)

	if err := server.user.Delete("INBOX"); err != nil {
		t.Fatalf("delete INBOX: %v", err)
	}
	if err := server.user.Create("INBOX", nil); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	server.append(mailSpec{messageID: "renumbered@example.com"}.raw())
	deadline := time.After(waitDeadline)
	for len(h.adapter.Check(t.Context())) == 0 {
		select {
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatal("no health finding after the mailbox identity changed")
		}
	}
	server.append(mailSpec{messageID: "after@example.com"}.raw())
	h.awaitEnvelopes(2)
	if ids := []string{h.sink.all()[0].ExternalID, h.sink.all()[1].ExternalID}; ids[1] != "email:work:after@example.com" {
		t.Errorf("ingested %v, want the mail after the change and none before it", ids)
	}
	if got := len(h.sink.all()); got != 2 {
		t.Errorf("envelopes = %d, want 2", got)
	}
}

func TestIMAP_NeverMarksSeenOrRemovesMail(t *testing.T) {
	server := startIMAPServer(t, rev1Caps)
	h := server.harness(5*time.Millisecond, true, harnessOptions{})
	h.startAndAwaitCursor()
	server.append(mailSpec{messageID: "keep@example.com"}.raw())
	h.awaitEnvelopes(1)
	h.stop()

	dialer := tls.Dialer{Config: &tls.Config{RootCAs: server.ca.pool, ServerName: server.host, MinVersion: tls.VersionTLS12}}
	conn, err := dialer.DialContext(t.Context(), "tcp", net.JoinHostPort(server.host, strconv.Itoa(server.port)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	client := imapclient.New(conn, nil)
	defer func() { _ = client.Close() }()
	if err := client.Login(imapUser, imapPassword).Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := client.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}
	buffers, err := client.Fetch(uidRange(1, 1), &imap.FetchOptions{UID: true, Flags: true}).Collect()
	if err != nil || len(buffers) != 1 {
		t.Fatalf("fetch = %d messages, %v; want the message still present", len(buffers), err)
	}
	for _, flag := range buffers[0].Flags {
		if flag == imap.FlagSeen || flag == imap.FlagDeleted {
			t.Errorf("mail carries the %s flag", flag)
		}
	}
}

func TestIMAP_SessionBoundsWhatItReads(t *testing.T) {
	server := startIMAPServer(t, rev1Caps)
	raw := mailSpec{messageID: "bounded@example.com", body: "0123456789012345678901234567890123456789"}.raw()
	server.append(raw)
	session, err := server.connector(server.account(time.Minute)).Connect(t.Context())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = session.Close() }()
	status, err := session.Select(t.Context(), "INBOX")
	if err != nil || status.UIDValidity == 0 || status.UIDNext != 2 {
		t.Fatalf("Select() = %+v, %v; want uid next 2", status, err)
	}
	infos, err := session.List(t.Context(), 1, 10)
	if err != nil || len(infos) != 1 || infos[0].UID != 1 || infos[0].Size != int64(len(raw)) {
		t.Fatalf("List() = %+v, %v", infos, err)
	}
	if empty, err := session.List(t.Context(), 5, 10); err != nil || len(empty) != 0 {
		t.Fatalf("List(empty range) = %+v, %v", empty, err)
	}
	header, err := session.Header(t.Context(), 1, 20)
	if err != nil || len(header) != 21 {
		t.Fatalf("Header(limit 20) = %d bytes, %v; want the limit plus one", len(header), err)
	}
	whole, err := session.Header(t.Context(), 1, maxHeaderBytes)
	if err != nil || !bytes.HasSuffix(whole, []byte("\r\n\r\n")) || bytes.Contains(whole, []byte("0123456789")) {
		t.Fatalf("Header() = %q, %v; want only the header section", whole, err)
	}
	message, err := session.Message(t.Context(), 1, 30)
	if err != nil || len(message) != 31 {
		t.Fatalf("Message(limit 30) = %d bytes, %v; want the limit plus one", len(message), err)
	}
	full, err := session.Message(t.Context(), 1, int64(len(raw)))
	if err != nil || !bytes.Equal(full, raw) {
		t.Fatalf("Message() = %q, %v; want the whole message", full, err)
	}
	_, err = session.Header(t.Context(), 99, maxHeaderBytes)
	if code, ok := CodeOf(err); !ok || code != ErrorCodeMessageUnavailable {
		t.Fatalf("Header(missing) error = %v, want message_unavailable", err)
	}
	if _, err := session.Select(t.Context(), "No such folder"); err == nil {
		t.Fatal("Select() accepted a missing folder")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeProtocolInvalid {
		t.Fatalf("Select(missing) error = %v, want protocol_invalid", err)
	}
}

func TestIMAP_ConnectFailuresAreClassified(t *testing.T) {
	server := startIMAPServer(t, rev1Caps)
	cases := map[string]struct {
		prepare func(t *testing.T, account *config.EmailAccount) *imapConnector
		want    ErrorCode
	}{
		"wrong password": {
			prepare: func(t *testing.T, account *config.EmailAccount) *imapConnector {
				t.Setenv("AURA_TEST_WRONG_PASSWORD", "wrong")
				account.IMAP.PasswordRef = "env://AURA_TEST_WRONG_PASSWORD"
				return server.connector(account)
			},
			want: ErrorCodeLoginRejected,
		},
		"unset password": {
			prepare: func(t *testing.T, account *config.EmailAccount) *imapConnector {
				account.IMAP.PasswordRef = "env://AURA_TEST_UNSET_PASSWORD"
				return server.connector(account)
			},
			want: ErrorCodeCredentialsUnavailable,
		},
		"missing password file": {
			prepare: func(t *testing.T, account *config.EmailAccount) *imapConnector {
				account.IMAP.PasswordRef = "file://" + t.TempDir() + "/absent"
				return server.connector(account)
			},
			want: ErrorCodeCredentialsUnavailable,
		},
		"untrusted certificate authority": {
			prepare: func(t *testing.T, account *config.EmailAccount) *imapConnector {
				connector, err := newIMAPConnector(account, x509.NewCertPool())
				if err != nil {
					t.Fatal(err)
				}
				return connector
			},
			want: ErrorCodeConnectionFailed,
		},
		"system roots do not trust the test authority": {
			prepare: func(t *testing.T, account *config.EmailAccount) *imapConnector {
				connector, err := newIMAPConnector(account, nil)
				if err != nil {
					t.Fatal(err)
				}
				return connector
			},
			want: ErrorCodeConnectionFailed,
		},
		"certificate for another host name": {
			prepare: func(t *testing.T, account *config.EmailAccount) *imapConnector {
				account.IMAP.Host = "localhost"
				return server.connector(account)
			},
			want: ErrorCodeConnectionFailed,
		},
		"nothing is listening": {
			prepare: func(t *testing.T, account *config.EmailAccount) *imapConnector {
				listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				account.IMAP.Port = listenerPort(t, listener)
				_ = listener.Close()
				return server.connector(account)
			},
			want: ErrorCodeConnectionFailed,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			connector := tc.prepare(t, server.account(time.Minute))
			session, err := connector.Connect(t.Context())
			if session != nil {
				_ = session.Close()
			}
			if code, ok := CodeOf(err); !ok || code != tc.want {
				t.Fatalf("Connect() error = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestIMAP_PlaintextServerIsNeverSpoken(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	received := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			received <- nil
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "* OK plaintext IMAP ready\r\n")
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 512)
		n, _ := conn.Read(buf)
		received <- buf[:n]
	}()
	t.Setenv(imapPasswordName, imapPassword)
	account := &config.EmailAccount{IMAP: config.EmailIMAP{
		Host: "127.0.0.1", Port: listenerPort(t, listener), Username: imapUser, PasswordRef: "env://" + imapPasswordName,
	}}
	connector, err := NewIMAPConnector(account)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connector.Connect(t.Context()); err == nil {
		t.Fatal("Connect() spoke IMAP to a plaintext server")
	}
	if got := <-received; bytes.Contains(got, []byte("LOGIN")) || bytes.Contains(got, []byte(imapPassword)) {
		t.Fatalf("plaintext server received %q", got)
	}
}

func TestNewIMAPConnector_RejectsBadInput(t *testing.T) {
	if _, err := NewIMAPConnector(nil); err == nil {
		t.Error("NewIMAPConnector(nil) succeeded")
	}
	for _, ref := range []string{"", "hunter2", "http://secret", "plain:hunter2"} {
		account := &config.EmailAccount{IMAP: config.EmailIMAP{Host: "imap.example.com", Port: 993, PasswordRef: ref}}
		_, err := NewIMAPConnector(account)
		if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
			t.Errorf("NewIMAPConnector(%q) error = %v, want invalid_argument", ref, err)
		}
	}
}

func TestIMAP_ConnectHonoursCancellation(t *testing.T) {
	server := startIMAPServer(t, rev1Caps)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := server.connector(server.account(time.Minute)).Connect(ctx); err == nil {
		t.Fatal("Connect() succeeded with a cancelled context")
	}
}

func listenerPort(t *testing.T, listener net.Listener) int {
	t.Helper()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("listener address: %v", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("listener port: %v", err)
	}
	return number
}

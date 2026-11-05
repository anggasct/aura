package email

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
)

const (
	smtpPasswordEnv = "AURA_TEST_SMTP_PASSWORD"
	smtpPassword    = "smtp-secret-probe-7f3a"
	smtpUsername    = "aura-submit"
)

type testAuthority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newAuthority(t *testing.T) *testAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate authority key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "aura test authority"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create authority: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse authority: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testAuthority{cert: cert, key: key, pool: pool}
}

func (a *testAuthority) issue(t *testing.T, ips []net.IP, names []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "aura test server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     names,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatalf("create server certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

type smtpBehavior struct {
	refuseGreeting bool
	silent         bool
	noStartTLS     bool
	noAuth         bool
	authBeforeTLS  bool
	authReply      string
	mailReply      string
	startTLSReply  string
	rcptReply      string
	dataReply      string
	finalReply     string
	afterData      string
	handshake      string
	certificate    *tls.Certificate
}

type receivedMail struct {
	from string
	to   []string
	data string
}

type smtpConnection struct {
	commands      []string
	tls           bool
	plaintextAuth bool
	authenticated bool
	dataStarted   bool
}

type fakeSMTP struct {
	t           *testing.T
	listener    net.Listener
	certificate tls.Certificate
	done        chan struct{}
	wg          sync.WaitGroup

	mu          sync.Mutex
	queue       []smtpBehavior
	connections []*smtpConnection
	mails       []receivedMail
}

func newFakeSMTP(t *testing.T, authority *testAuthority) *fakeSMTP {
	t.Helper()
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &fakeSMTP{
		t:           t,
		listener:    listener,
		certificate: authority.issue(t, []net.IP{net.ParseIP("127.0.0.1")}, []string{"localhost"}),
		done:        make(chan struct{}),
	}
	server.wg.Add(1)
	go server.accept()
	t.Cleanup(func() {
		close(server.done)
		_ = listener.Close()
		server.wg.Wait()
	})
	return server
}

func (s *fakeSMTP) port() int {
	return int(netip.MustParseAddrPort(s.listener.Addr().String()).Port())
}

func (s *fakeSMTP) script(behaviors ...smtpBehavior) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, behaviors...)
}

func (s *fakeSMTP) received() []receivedMail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]receivedMail(nil), s.mails...)
}

func (s *fakeSMTP) sessions() []smtpConnection {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]smtpConnection, 0, len(s.connections))
	for _, connection := range s.connections {
		copied := *connection
		copied.commands = append([]string(nil), connection.commands...)
		out = append(out, copied)
	}
	return out
}

func (s *fakeSMTP) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		behavior := smtpBehavior{}
		if len(s.queue) > 0 {
			behavior = s.queue[0]
			s.queue = s.queue[1:]
		}
		connection := &smtpConnection{}
		s.connections = append(s.connections, connection)
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(conn, &behavior, connection)
		}()
	}
}

func replyOr(reply, fallback string) string {
	if reply == "" {
		return fallback
	}
	return reply
}

func (s *fakeSMTP) update(connection *smtpConnection, change func(*smtpConnection)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(connection)
}

func (s *fakeSMTP) serve(raw net.Conn, behavior *smtpBehavior, connection *smtpConnection) {
	conn := raw
	defer func() { _ = conn.Close() }()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-s.done:
			_ = conn.Close()
		case <-stop:
		}
	}()
	if behavior.refuseGreeting {
		return
	}
	if behavior.silent {
		<-s.done
		return
	}
	reader := bufio.NewReader(conn)
	say := func(line string) { _, _ = fmt.Fprintf(conn, "%s\r\n", line) }
	say("220 fake ESMTP ready")
	var from string
	var to []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, rest, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		recorded := line
		if verb == "AUTH" {
			recorded = "AUTH"
		}
		s.update(connection, func(c *smtpConnection) { c.commands = append(c.commands, recorded) })
		switch verb {
		case "EHLO", "HELO":
			s.ehlo(conn, behavior, connection)
		case "STARTTLS":
			if behavior.startTLSReply != "" {
				say(behavior.startTLSReply)
				continue
			}
			say("220 go ahead")
			certificate := s.certificate
			if behavior.certificate != nil {
				certificate = *behavior.certificate
			}
			config := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
			switch behavior.handshake {
			case "garbage":
				_, _ = conn.Write([]byte("this is not a tls record\r\n"))
				return
			case "stall":
				<-s.done
				return
			case "alert":
				config.MinVersion = tls.VersionTLS10
				config.MaxVersion = tls.VersionTLS10
			}
			secured := tls.Server(conn, config)
			if err := secured.HandshakeContext(s.t.Context()); err != nil {
				return
			}
			conn = secured
			reader = bufio.NewReader(conn)
			s.update(connection, func(c *smtpConnection) { c.tls = true })
		case "AUTH":
			s.update(connection, func(c *smtpConnection) {
				if !c.tls {
					c.plaintextAuth = true
				}
			})
			reply := behavior.authReply
			if reply == "" && authenticates(rest) {
				reply = "235 2.7.0 authenticated"
			}
			if reply == "" {
				reply = "535 5.7.8 credentials rejected"
			}
			if strings.HasPrefix(reply, "235") {
				s.update(connection, func(c *smtpConnection) { c.authenticated = true })
			}
			say(reply)
		case "MAIL":
			from = rest
			to = nil
			say(replyOr(behavior.mailReply, "250 2.1.0 ok"))
		case "RCPT":
			reply := replyOr(behavior.rcptReply, "250 2.1.5 ok")
			if strings.HasPrefix(reply, "250") {
				to = append(to, rest)
			}
			say(reply)
		case "DATA":
			reply := replyOr(behavior.dataReply, "354 end data with <CR><LF>.<CR><LF>")
			say(reply)
			if !strings.HasPrefix(reply, "354") {
				continue
			}
			s.update(connection, func(c *smtpConnection) { c.dataStarted = true })
			data, err := readData(reader)
			if err != nil {
				return
			}
			s.mu.Lock()
			s.mails = append(s.mails, receivedMail{from: from, to: to, data: data})
			s.mu.Unlock()
			switch behavior.afterData {
			case "drop":
				return
			case "stall":
				<-s.done
				return
			}
			say(replyOr(behavior.finalReply, "250 2.0.0 queued"))
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 5.5.2 command not recognized")
		}
	}
}

func (s *fakeSMTP) ehlo(conn net.Conn, behavior *smtpBehavior, connection *smtpConnection) {
	s.mu.Lock()
	secured := connection.tls
	s.mu.Unlock()
	lines := []string{"250-fake.example.test"}
	if !behavior.noStartTLS && !secured {
		lines = append(lines, "250-STARTTLS")
	}
	if !behavior.noAuth && (secured || behavior.authBeforeTLS) {
		lines = append(lines, "250-AUTH PLAIN")
	}
	lines = append(lines, "250 8BITMIME")
	for _, line := range lines {
		_, _ = fmt.Fprintf(conn, "%s\r\n", line)
	}
}

func authenticates(rest string) bool {
	mechanism, encoded, _ := strings.Cut(rest, " ")
	if !strings.EqualFold(mechanism, "PLAIN") {
		return false
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	fields := bytes.Split(decoded, []byte{0})
	return len(fields) == 3 && string(fields[1]) == smtpUsername && string(fields[2]) == smtpPassword
}

func readData(reader *bufio.Reader) (string, error) {
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		if line == ".\r\n" {
			return data.String(), nil
		}
		data.WriteString(strings.TrimPrefix(line, "."))
	}
}

func smtpAccount(server *fakeSMTP) *config.EmailAccount {
	return &config.EmailAccount{
		Name:       testAccount,
		Persona:    testPersona,
		Address:    testAddress,
		AuthservID: testAuthserv,
		IMAP:       config.EmailIMAP{Host: "imap.example.com", Port: 993, Username: testAddress, PasswordRef: "env://AURA_TEST_EMAIL_PASSWORD"},
		SMTP: config.EmailSMTP{
			Host:        "127.0.0.1",
			Port:        server.port(),
			Username:    smtpUsername,
			PasswordRef: "env://" + smtpPasswordEnv,
		},
	}
}

func newTestMailer(t *testing.T, server *fakeSMTP, authority *testAuthority) *smtpMailer {
	t.Helper()
	t.Setenv(smtpPasswordEnv, smtpPassword)
	mailer, err := newSMTPMailer(smtpAccount(server), authority.pool)
	if err != nil {
		t.Fatalf("newSMTPMailer: %v", err)
	}
	mailer.stepTimeout = 5 * time.Second
	mailer.attemptTimeout = 20 * time.Second
	return mailer
}

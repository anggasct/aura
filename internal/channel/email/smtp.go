package email

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/secret"
)

const (
	smtpStepTimeout    = 30 * time.Second
	smtpAttemptTimeout = 2 * time.Minute
	smtpPermanentClass = 5
	smtpClassDivisor   = 100
)

type Submission struct {
	From    string
	To      string
	Message []byte
}

type Mailer interface {
	Submit(ctx context.Context, submission *Submission) error
}

type smtpMailer struct {
	address        string
	host           string
	hello          string
	username       string
	password       secret.Reference
	roots          *x509.CertPool
	stepTimeout    time.Duration
	attemptTimeout time.Duration
}

func NewSMTPMailer(account *config.EmailAccount) (Mailer, error) {
	return newSMTPMailer(account, nil)
}

func newSMTPMailer(account *config.EmailAccount, roots *x509.CertPool) (*smtpMailer, error) {
	if account == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "email account must not be nil")
	}
	password, err := parsePasswordRef(account.SMTP.PasswordRef)
	if err != nil {
		return nil, err
	}
	return &smtpMailer{
		address:        net.JoinHostPort(account.SMTP.Host, strconv.Itoa(account.SMTP.Port)),
		host:           account.SMTP.Host,
		hello:          domainOf(account.Address),
		username:       account.SMTP.Username,
		password:       password,
		roots:          roots,
		stepTimeout:    smtpStepTimeout,
		attemptTimeout: smtpAttemptTimeout,
	}, nil
}

type stepConn struct {
	net.Conn
	timeout time.Duration
}

func (c *stepConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *stepConn) Write(p []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func (m *smtpMailer) Submit(ctx context.Context, submission *Submission) error {
	if submission == nil {
		return Errorf(ErrorCodeInvalidArgument, "submission must not be nil")
	}
	password, err := m.password.Resolve()
	if err != nil || strings.TrimSpace(password) == "" {
		return Errorf(ErrorCodeCredentialsUnavailable, "submission password is unavailable")
	}
	call, cancel := context.WithTimeout(ctx, m.attemptTimeout)
	defer cancel()
	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(call, "tcp", m.address)
	if err != nil {
		return refusal(ErrorCodeConnectionFailed, "submission server is unreachable", err)
	}
	defer context.AfterFunc(call, func() { _ = conn.Close() })()
	client, err := smtp.NewClient(&stepConn{Conn: conn, timeout: m.stepTimeout}, m.host)
	if err != nil {
		_ = conn.Close()
		return classify(ErrorCodeSubmissionRejected, err)
	}
	defer func() { _ = client.Close() }()
	if err := m.secure(client, password); err != nil {
		return err
	}
	return transfer(client, submission)
}

func (m *smtpMailer) secure(client *smtp.Client, password string) error {
	if err := client.Hello(m.hello); err != nil {
		return classify(ErrorCodeSubmissionRejected, err)
	}
	if offered, _ := client.Extension("STARTTLS"); !offered {
		return Errorf(ErrorCodeTLSRequired, "submission server does not offer STARTTLS")
	}
	if err := client.StartTLS(&tls.Config{ServerName: m.host, MinVersion: tls.VersionTLS12, RootCAs: m.roots}); err != nil {
		return classify(ErrorCodeTLSRequired, err)
	}
	if state, encrypted := client.TLSConnectionState(); !encrypted || !state.HandshakeComplete {
		return Errorf(ErrorCodeTLSRequired, "submission connection is not encrypted")
	}
	if offered, _ := client.Extension("AUTH"); !offered {
		return Errorf(ErrorCodeLoginRejected, "submission server offers no authentication")
	}
	if err := client.Auth(smtp.PlainAuth("", m.username, password, m.host)); err != nil {
		return classify(ErrorCodeLoginRejected, err)
	}
	return nil
}

func transfer(client *smtp.Client, submission *Submission) error {
	if err := client.Mail(submission.From); err != nil {
		return classify(ErrorCodeSubmissionRejected, err)
	}
	if err := client.Rcpt(submission.To); err != nil {
		return classify(ErrorCodeSubmissionRejected, err)
	}
	writer, err := client.Data()
	if err != nil {
		return classify(ErrorCodeSubmissionRejected, err)
	}
	if _, err := writer.Write(submission.Message); err != nil {
		return refusal(ErrorCodeDeliveryAmbiguous, "submission outcome is unknown", err)
	}
	if err := writer.Close(); err != nil {
		var reply *textproto.Error
		if errors.As(err, &reply) && reply.Code/smtpClassDivisor == smtpPermanentClass {
			return Errorf(ErrorCodeSubmissionRejected, "submission server rejected the message")
		}
		return Errorf(ErrorCodeDeliveryAmbiguous, "submission outcome is unknown")
	}
	_ = client.Quit()
	return nil
}

func classify(permanent ErrorCode, err error) error {
	var reply *textproto.Error
	if errors.As(err, &reply) {
		if reply.Code/smtpClassDivisor == smtpPermanentClass {
			return Errorf(permanent, "submission server refused the command")
		}
		return Errorf(ErrorCodeSubmissionDeferred, "submission server deferred the command")
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verification *tls.CertificateVerificationError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &verification) {
		return &Error{Code: ErrorCodeTLSUntrusted, Detail: "submission server certificate is not trusted", Cause: err}
	}
	return refusal(ErrorCodeConnectionFailed, "submission connection failed", err)
}

func refusal(code ErrorCode, detail string, cause error) error {
	return &Error{Code: code, Detail: detail, Cause: cause}
}

package email

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/secret"
)

const dialTimeout = 30 * time.Second

type imapConnector struct {
	address  string
	host     string
	username string
	password secret.Reference
	roots    *x509.CertPool
}

func NewIMAPConnector(account *config.EmailAccount) (Connector, error) {
	return newIMAPConnector(account, nil)
}

func newIMAPConnector(account *config.EmailAccount, roots *x509.CertPool) (*imapConnector, error) {
	if account == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "email account must not be nil")
	}
	password, err := parsePasswordRef(account.IMAP.PasswordRef)
	if err != nil {
		return nil, err
	}
	return &imapConnector{
		address:  net.JoinHostPort(account.IMAP.Host, strconv.Itoa(account.IMAP.Port)),
		host:     account.IMAP.Host,
		username: account.IMAP.Username,
		password: password,
		roots:    roots,
	}, nil
}

func (c *imapConnector) Connect(ctx context.Context) (Session, error) {
	password, err := c.password.Resolve()
	if err != nil || strings.TrimSpace(password) == "" {
		return nil, Errorf(ErrorCodeCredentialsUnavailable, "mailbox password is unavailable")
	}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: dialTimeout},
		Config:    &tls.Config{ServerName: c.host, MinVersion: tls.VersionTLS12, RootCAs: c.roots},
	}
	conn, err := dialer.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return nil, &Error{Code: ErrorCodeConnectionFailed, Detail: "mailbox server is unreachable", Cause: err}
	}
	updates := make(chan struct{}, 1)
	client := imapclient.New(conn, &imapclient.Options{UnilateralDataHandler: &imapclient.UnilateralDataHandler{
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages == nil {
				return
			}
			select {
			case updates <- struct{}{}:
			default:
			}
		},
	}})
	session := &imapSession{client: client, updates: updates}
	defer session.guard(ctx)()
	if err := client.Login(c.username, password).Wait(); err != nil {
		_ = client.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var rejected *imap.Error
		if errors.As(err, &rejected) {
			return nil, Errorf(ErrorCodeLoginRejected, "mailbox server rejected the credentials")
		}
		return nil, &Error{Code: ErrorCodeConnectionFailed, Detail: "mailbox login did not complete", Cause: err}
	}
	session.idle = supportsIdle(client.Caps())
	return session, nil
}

type imapSession struct {
	client  *imapclient.Client
	updates chan struct{}
	idle    bool
}

func (s *imapSession) guard(ctx context.Context) func() {
	stop := context.AfterFunc(ctx, func() { _ = s.client.Close() })
	return func() { stop() }
}

func (s *imapSession) failure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var refused *imap.Error
	if errors.As(err, &refused) {
		return &Error{Code: ErrorCodeProtocolInvalid, Detail: "mailbox server refused the request", Cause: err}
	}
	return &Error{Code: ErrorCodeConnectionFailed, Detail: "mailbox connection failed", Cause: err}
}

func (s *imapSession) Select(ctx context.Context, folder string) (MailboxStatus, error) {
	defer s.guard(ctx)()
	data, err := s.client.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return MailboxStatus{}, s.failure(ctx, err)
	}
	return MailboxStatus{UIDValidity: data.UIDValidity, UIDNext: uint32(data.UIDNext)}, nil
}

func (s *imapSession) List(ctx context.Context, first, last uint32) ([]MessageInfo, error) {
	defer s.guard(ctx)()
	buffers, err := s.client.Fetch(uidRange(first, last), &imap.FetchOptions{UID: true, RFC822Size: true}).Collect()
	if err != nil {
		return nil, s.failure(ctx, err)
	}
	infos := make([]MessageInfo, 0, len(buffers))
	for _, buffer := range buffers {
		if buffer.UID != 0 {
			infos = append(infos, MessageInfo{UID: uint32(buffer.UID), Size: buffer.RFC822Size})
		}
	}
	return infos, nil
}

func (s *imapSession) Header(ctx context.Context, uid uint32, limit int64) ([]byte, error) {
	return s.section(ctx, uid, &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true, Partial: &imap.SectionPartial{Size: limit + 1}}, limit)
}

func (s *imapSession) Message(ctx context.Context, uid uint32, limit int64) ([]byte, error) {
	return s.section(ctx, uid, &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Size: limit + 1}}, limit)
}

func (s *imapSession) section(ctx context.Context, uid uint32, section *imap.FetchItemBodySection, limit int64) ([]byte, error) {
	defer s.guard(ctx)()
	command := s.client.Fetch(uidRange(uid, uid), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{section}})
	var content []byte
	found := false
	for message := command.Next(); message != nil; message = command.Next() {
		for item := message.Next(); item != nil; item = message.Next() {
			body, ok := item.(imapclient.FetchItemDataBodySection)
			if !ok || body.Literal == nil {
				continue
			}
			data, err := io.ReadAll(io.LimitReader(body.Literal, limit+1))
			if err != nil {
				_ = command.Close()
				return nil, s.failure(ctx, err)
			}
			content, found = data, true
		}
	}
	if err := command.Close(); err != nil {
		return nil, s.failure(ctx, err)
	}
	if !found {
		return nil, Errorf(ErrorCodeMessageUnavailable, "message is no longer in the mailbox")
	}
	return content, nil
}

func (s *imapSession) Wait(ctx context.Context, interval time.Duration) error {
	if !s.idle {
		if !sleepContext(ctx, interval) {
			return ctx.Err()
		}
		return nil
	}
	defer s.guard(ctx)()
	command, err := s.client.Idle()
	if err != nil {
		return s.failure(ctx, err)
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-s.updates:
	case <-timer.C:
	case <-s.client.Closed():
	}
	closeErr := command.Close()
	waitErr := command.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := errors.Join(closeErr, waitErr); err != nil {
		return s.failure(ctx, err)
	}
	return nil
}

func (s *imapSession) Close() error {
	return s.client.Close()
}

func supportsIdle(caps imap.CapSet) bool {
	return caps.Has(imap.CapIdle) || caps.Has(imap.CapIMAP4rev2)
}

func uidRange(first, last uint32) imap.UIDSet {
	var set imap.UIDSet
	set.AddRange(imap.UID(first), imap.UID(last))
	return set
}

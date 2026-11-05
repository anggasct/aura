package email

import (
	"context"
	"crypto/tls"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

const shortTimeout = 300 * time.Millisecond

func wireSubmission(t *testing.T) *Submission {
	t.Helper()
	reply := baseReply()
	reply.Body = "first line\n.\n..dotted\nlast line"
	raw, err := reply.render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return &Submission{From: testAddress, To: ownerAddress, Message: raw}
}

func commandVerbs(connection *smtpConnection) []string {
	verbs := make([]string, 0, len(connection.commands))
	for _, command := range connection.commands {
		verb, _, _ := strings.Cut(command, " ")
		verbs = append(verbs, strings.ToUpper(verb))
	}
	return verbs
}

func wantCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if code, ok := CodeOf(err); !ok || code != want {
		t.Fatalf("error = %v, want code %s", err, want)
	}
}

func TestSMTPMailer_SubmitsOverVerifiedSTARTTLS(t *testing.T) {
	authority := newAuthority(t)
	server := newFakeSMTP(t, authority)
	mailer := newTestMailer(t, server, authority)
	submission := wireSubmission(t)
	if err := mailer.Submit(t.Context(), submission); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	mails := server.received()
	if len(mails) != 1 {
		t.Fatalf("server received %d messages, want 1", len(mails))
	}
	if !strings.HasPrefix(mails[0].from, "FROM:<"+testAddress+">") || !slices.Equal(mails[0].to, []string{"TO:<" + ownerAddress + ">"}) {
		t.Errorf("envelope = %q -> %q", mails[0].from, mails[0].to)
	}
	if mails[0].data != string(submission.Message) {
		t.Errorf("message on the wire differs from the submitted message (dot-stuffing must round-trip):\n%q", mails[0].data)
	}
	sessions := server.sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	verbs := commandVerbs(&sessions[0])
	if tlsAt, authAt := slices.Index(verbs, "STARTTLS"), slices.Index(verbs, "AUTH"); tlsAt < 0 || authAt < tlsAt {
		t.Errorf("commands = %v, want STARTTLS before AUTH", verbs)
	}
	if !sessions[0].tls || !sessions[0].authenticated || sessions[0].plaintextAuth {
		t.Errorf("session = %+v, want authentication only inside TLS", sessions[0])
	}
	if sessions[0].commands[0] != "EHLO example.com" {
		t.Errorf("first command = %q, want EHLO in the account domain", sessions[0].commands[0])
	}
}

func TestSMTPMailer_RefusesWithoutSTARTTLSBeforeAuth(t *testing.T) {
	authority := newAuthority(t)
	server := newFakeSMTP(t, authority)
	server.script(smtpBehavior{noStartTLS: true, authBeforeTLS: true})
	mailer := newTestMailer(t, server, authority)
	err := mailer.Submit(t.Context(), wireSubmission(t))
	wantCode(t, err, ErrorCodeTLSRequired)
	sessions := server.sessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if slices.Contains(commandVerbs(&sessions[0]), "AUTH") || sessions[0].plaintextAuth || sessions[0].tls {
		t.Errorf("session = %+v (%v), want no AUTH and no TLS", sessions[0], commandVerbs(&sessions[0]))
	}
	if len(server.received()) != 0 {
		t.Error("a message crossed an unencrypted connection")
	}
}

func TestSMTPMailer_RefusesUntrustedCertificates(t *testing.T) {
	authority := newAuthority(t)
	stranger := newAuthority(t)
	cases := map[string]func(t *testing.T, server *fakeSMTP) tls.Certificate{
		"unknown authority": func(t *testing.T, _ *fakeSMTP) tls.Certificate {
			return stranger.issue(t, []net.IP{net.ParseIP("127.0.0.1")}, []string{"localhost"})
		},
		"wrong host name": func(t *testing.T, _ *fakeSMTP) tls.Certificate {
			return authority.issue(t, nil, []string{"other.example"})
		},
	}
	for name, issue := range cases {
		t.Run(name, func(t *testing.T) {
			server := newFakeSMTP(t, authority)
			certificate := issue(t, server)
			server.script(smtpBehavior{certificate: &certificate})
			mailer := newTestMailer(t, server, authority)
			wantCode(t, mailer.Submit(t.Context(), wireSubmission(t)), ErrorCodeTLSUntrusted)
			for _, session := range server.sessions() {
				if session.plaintextAuth || session.authenticated || slices.Contains(commandVerbs(&session), "AUTH") {
					t.Errorf("credentials were offered to an unverified server: %+v", session)
				}
			}
			if len(server.received()) != 0 {
				t.Error("a message reached an unverified server")
			}
		})
	}
}

func TestNewSMTPMailer_TrustsOnlySystemRoots(t *testing.T) {
	authority := newAuthority(t)
	server := newFakeSMTP(t, authority)
	t.Setenv(smtpPasswordEnv, smtpPassword)
	mailer, err := NewSMTPMailer(smtpAccount(server))
	if err != nil {
		t.Fatalf("NewSMTPMailer: %v", err)
	}
	wantCode(t, mailer.Submit(t.Context(), wireSubmission(t)), ErrorCodeTLSUntrusted)
	if len(server.received()) != 0 {
		t.Error("a message reached a server outside the system trust store")
	}
}

func TestNewSMTPMailer_RejectsBadInput(t *testing.T) {
	_, err := NewSMTPMailer(nil)
	wantCode(t, err, ErrorCodeInvalidArgument)
	server := newFakeSMTP(t, newAuthority(t))
	account := smtpAccount(server)
	account.SMTP.PasswordRef = "hunter2"
	_, err = NewSMTPMailer(account)
	wantCode(t, err, ErrorCodeInvalidArgument)
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q leaks the inline password", err)
	}
	mailer, err := newSMTPMailer(smtpAccount(server), nil)
	if err != nil {
		t.Fatalf("newSMTPMailer: %v", err)
	}
	wantCode(t, mailer.Submit(t.Context(), nil), ErrorCodeInvalidArgument)
}

func TestSMTPMailer_ClassifiesFailuresBeforeData(t *testing.T) {
	cases := map[string]struct {
		behavior smtpBehavior
		want     ErrorCode
	}{
		"credentials rejected":      {behavior: smtpBehavior{authReply: "535 5.7.8 no"}, want: ErrorCodeLoginRejected},
		"credentials deferred":      {behavior: smtpBehavior{authReply: "454 4.7.0 later"}, want: ErrorCodeSubmissionDeferred},
		"no authentication offered": {behavior: smtpBehavior{noAuth: true}, want: ErrorCodeLoginRejected},
		"sender refused":            {behavior: smtpBehavior{mailReply: "550 5.7.1 no"}, want: ErrorCodeSubmissionRejected},
		"sender deferred":           {behavior: smtpBehavior{mailReply: "451 4.3.0 busy"}, want: ErrorCodeSubmissionDeferred},
		"recipient refused":         {behavior: smtpBehavior{rcptReply: "550 5.1.1 owner@example.com unknown"}, want: ErrorCodeSubmissionRejected},
		"recipient deferred":        {behavior: smtpBehavior{rcptReply: "451 4.2.0 owner@example.com later"}, want: ErrorCodeSubmissionDeferred},
		"data refused":              {behavior: smtpBehavior{dataReply: "554 5.3.0 no"}, want: ErrorCodeSubmissionRejected},
		"data deferred":             {behavior: smtpBehavior{dataReply: "451 4.3.0 later"}, want: ErrorCodeSubmissionDeferred},
		"connection dropped":        {behavior: smtpBehavior{refuseGreeting: true}, want: ErrorCodeConnectionFailed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			authority := newAuthority(t)
			server := newFakeSMTP(t, authority)
			server.script(tc.behavior)
			mailer := newTestMailer(t, server, authority)
			err := mailer.Submit(t.Context(), wireSubmission(t))
			wantCode(t, err, tc.want)
			for _, leak := range []string{ownerAddress, smtpPassword, "unknown", "later"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q carries %q", err, leak)
				}
			}
			if len(server.received()) != 0 {
				t.Error("a message was accepted")
			}
		})
	}
}

func TestSMTPMailer_BoundsEveryStepWithADeadline(t *testing.T) {
	authority := newAuthority(t)
	server := newFakeSMTP(t, authority)
	server.script(smtpBehavior{silent: true})
	mailer := newTestMailer(t, server, authority)
	mailer.stepTimeout = shortTimeout
	wantCode(t, mailer.Submit(t.Context(), wireSubmission(t)), ErrorCodeConnectionFailed)
}

func TestSMTPMailer_AttemptDeadlineCoversAHungServer(t *testing.T) {
	authority := newAuthority(t)
	server := newFakeSMTP(t, authority)
	server.script(smtpBehavior{silent: true})
	mailer := newTestMailer(t, server, authority)
	mailer.attemptTimeout = shortTimeout
	wantCode(t, mailer.Submit(t.Context(), wireSubmission(t)), ErrorCodeConnectionFailed)
}

func TestSMTPMailer_CancelledContextFailsBeforeAnyWire(t *testing.T) {
	authority := newAuthority(t)
	server := newFakeSMTP(t, authority)
	mailer := newTestMailer(t, server, authority)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	wantCode(t, mailer.Submit(ctx, wireSubmission(t)), ErrorCodeConnectionFailed)
	if len(server.received()) != 0 {
		t.Error("a cancelled submission delivered a message")
	}
}

func TestSMTPMailer_MissingPasswordNeverConnects(t *testing.T) {
	authority := newAuthority(t)
	server := newFakeSMTP(t, authority)
	mailer := newTestMailer(t, server, authority)
	t.Setenv(smtpPasswordEnv, "")
	wantCode(t, mailer.Submit(t.Context(), wireSubmission(t)), ErrorCodeCredentialsUnavailable)
	if len(server.sessions()) != 0 {
		t.Error("the mailer connected without a password")
	}
}

func TestSMTPMailer_FailureAfterDataIsAmbiguousOrRejectedNeverRetriedHere(t *testing.T) {
	cases := map[string]struct {
		behavior smtpBehavior
		want     ErrorCode
	}{
		"connection dropped after the body":  {behavior: smtpBehavior{afterData: "drop"}, want: ErrorCodeDeliveryAmbiguous},
		"no reply after the body":            {behavior: smtpBehavior{afterData: "stall"}, want: ErrorCodeDeliveryAmbiguous},
		"temporary failure after the body":   {behavior: smtpBehavior{finalReply: "451 4.3.0 try later"}, want: ErrorCodeDeliveryAmbiguous},
		"permanent rejection after the body": {behavior: smtpBehavior{finalReply: "554 5.6.0 refused"}, want: ErrorCodeSubmissionRejected},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			authority := newAuthority(t)
			server := newFakeSMTP(t, authority)
			server.script(tc.behavior)
			mailer := newTestMailer(t, server, authority)
			mailer.stepTimeout = shortTimeout
			err := mailer.Submit(t.Context(), wireSubmission(t))
			wantCode(t, err, tc.want)
			if len(server.received()) != 1 || len(server.sessions()) != 1 {
				t.Errorf("messages %d, sessions %d; want exactly one of each", len(server.received()), len(server.sessions()))
			}
			for _, leak := range []string{ownerAddress, "later", "refused"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q carries %q", err, leak)
				}
			}
		})
	}
}

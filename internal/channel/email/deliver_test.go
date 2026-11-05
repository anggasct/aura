package email

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	mimepkg "mime"

	"github.com/anggasct/aura/internal/effect"
	runtimechannelhost "github.com/anggasct/aura/internal/runtime/channelhost"
	"github.com/anggasct/aura/internal/store"
)

const deliverySession = "main/email:work/thread:abc"

var fixedNow = time.Date(2026, time.November, 5, 9, 30, 0, 0, time.UTC)

type deliveryFixture struct {
	t         *testing.T
	h         *harness
	server    *fakeSMTP
	authority *testAuthority
	mailer    *smtpMailer
}

func newDeliveryFixture(t *testing.T, behaviors ...smtpBehavior) *deliveryFixture {
	t.Helper()
	authority := newAuthority(t)
	server := newFakeSMTP(t, authority)
	server.script(behaviors...)
	mailer := newTestMailer(t, server, authority)
	h := newHarness(t, harnessOptions{mailer: mailer})
	h.adapter.now = func() time.Time { return fixedNow }
	return &deliveryFixture{t: t, h: h, server: server, authority: authority, mailer: mailer}
}

func (f *deliveryFixture) request(effectID string, reply *replyReference, texts ...string) *runtimechannelhost.DeliveryRequest {
	f.t.Helper()
	if err := (&storeSessions{sessions: store.NewSessionService(f.h.db)}).EnsureSession(f.t.Context(), deliverySession, "local-owner", testPersona); err != nil {
		f.t.Fatalf("EnsureSession: %v", err)
	}
	replyContext, err := json.Marshal(reply)
	if err != nil {
		f.t.Fatalf("marshal reply context: %v", err)
	}
	parts := make([]runtimechannelhost.OutputPart, 0, len(texts))
	for _, text := range texts {
		parts = append(parts, runtimechannelhost.OutputPart{Text: text})
	}
	return &runtimechannelhost.DeliveryRequest{
		EffectID:       effectID,
		Channel:        "email:work",
		ConversationID: deliverySession,
		ReplyContext:   replyContext,
		Parts:          parts,
		IdempotencyKey: effectID,
	}
}

func defaultReply() *replyReference {
	return &replyReference{MessageID: "m1@example.com", Sender: ownerAddress, Subject: "Plan the week", References: []string{"root@example.com"}}
}

type storeSessions struct {
	sessions store.SessionService
}

func (s *storeSessions) EnsureSession(ctx context.Context, sessionID, ownerID, agentID string) error {
	if _, err := s.sessions.Get(ctx, sessionID); err == nil {
		return nil
	}
	now := time.Now().UTC()
	err := s.sessions.Create(ctx, &store.Session{ID: sessionID, OwnerID: ownerID, AgentID: agentID, Metadata: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now})
	if code, ok := store.CodeOf(err); ok && code == store.ErrorCodeSessionIDConflict {
		return nil
	}
	return err
}

func (f *deliveryFixture) intents() map[effect.State][]string {
	f.t.Helper()
	rows, err := f.h.db.QueryContext(f.t.Context(), `SELECT state, COALESCE(safe_error_code, '') FROM effect_intent WHERE provider = ?`, ProviderName)
	if err != nil {
		f.t.Fatalf("query intents: %v", err)
	}
	defer func() { _ = rows.Close() }()
	states := map[effect.State][]string{}
	for rows.Next() {
		var state, code string
		if err := rows.Scan(&state, &code); err != nil {
			f.t.Fatalf("scan: %v", err)
		}
		states[effect.State(state)] = append(states[effect.State(state)], code)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatalf("iterate intents: %v", err)
	}
	return states
}

func (f *deliveryFixture) wantIntents(want map[effect.State][]string) {
	f.t.Helper()
	got := f.intents()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		f.t.Fatalf("intents = %v, want %v", got, want)
	}
}

func parseReceived(t *testing.T, received receivedMail) *mail.Message {
	t.Helper()
	parsed, err := mail.ReadMessage(strings.NewReader(received.data))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return parsed
}

func bodyOf(t *testing.T, parsed *mail.Message) string {
	t.Helper()
	body, err := io.ReadAll(parsed.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return strings.ReplaceAll(string(body), "\r\n", "\n")
}

func TestDeliver_RepliesToTheAuthenticatedSenderOncePerIntent(t *testing.T) {
	f := newDeliveryFixture(t)
	req := f.request("turn-1", defaultReply(), "Here is the agenda.")
	receipt, err := f.h.adapter.Deliver(t.Context(), req)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	mails := f.server.received()
	if len(mails) != 1 {
		t.Fatalf("server received %d messages, want 1", len(mails))
	}
	parsed := parseReceived(t, mails[0])
	wantID := replyMessageID("turn-1", "example.com")
	want := map[string]string{
		"From":           testAddress,
		"To":             ownerAddress,
		"Subject":        "Re: Plan the week",
		"Message-Id":     "<" + wantID + ">",
		"In-Reply-To":    "<m1@example.com>",
		"Auto-Submitted": "auto-replied",
		"Content-Type":   "text/plain; charset=utf-8",
		"Date":           "Thu, 05 Nov 2026 09:30:00 +0000",
	}
	for key, value := range want {
		if got := parsed.Header.Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if got := strings.Join(strings.Fields(parsed.Header.Get("References")), " "); got != "<root@example.com> <m1@example.com>" {
		t.Errorf("References = %q", got)
	}
	if body := bodyOf(t, parsed); body != "Here is the agenda.\n" {
		t.Errorf("body = %q", body)
	}
	if receipt.ProviderID != ProviderName || receipt.ExternalID != wantID || !receipt.At.Equal(fixedNow) {
		t.Errorf("receipt = %+v, want the deterministic message id", receipt)
	}
	if !strings.HasPrefix(mails[0].from, "FROM:<"+testAddress+">") || len(mails[0].to) != 1 || mails[0].to[0] != "TO:<"+ownerAddress+">" {
		t.Errorf("envelope = %q -> %q, want the account to the sender alone", mails[0].from, mails[0].to)
	}
	if again, err := f.h.adapter.Deliver(t.Context(), req); err != nil || again.ExternalID != wantID {
		t.Fatalf("redelivery = %+v, %v; want the recorded receipt", again, err)
	}
	if got := len(f.server.received()); got != 1 {
		t.Fatalf("redelivery sent %d more messages, want none", got-1)
	}
	f.wantIntents(map[effect.State][]string{effect.StateSucceeded: {""}})
}

func TestDeliver_NeverAddressesReplyToCcOrBcc(t *testing.T) {
	f := newDeliveryFixture(t)
	f.h.start()
	f.h.deliver(mailSpec{
		subject:   "Plan the week",
		messageID: "m1@example.com",
		extra:     []string{"Reply-To: attacker@evil.example", "Cc: bystander@example.org", "Sender: relay@evil.example"},
	}.raw())
	envelopes := f.h.sink.all()
	if len(envelopes) != 1 {
		t.Fatalf("envelopes = %d, want 1", len(envelopes))
	}
	req := f.request("turn-reply-to", nil, "answer")
	req.ReplyContext = envelopes[0].ReplyContext
	if _, err := f.h.adapter.Deliver(t.Context(), req); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	mails := f.server.received()
	if len(mails) != 1 || len(mails[0].to) != 1 || mails[0].to[0] != "TO:<"+ownerAddress+">" {
		t.Fatalf("recipients = %+v, want the authenticated sender alone", mails)
	}
	parsed := parseReceived(t, mails[0])
	for _, header := range []string{"Reply-To", "Cc", "Bcc", "Sender"} {
		if _, present := parsed.Header[header]; present {
			t.Errorf("header %s present", header)
		}
	}
	if strings.Contains(mails[0].data, "evil.example") || strings.Contains(mails[0].data, "bystander") {
		t.Error("the message mentions an unauthenticated address")
	}
}

func TestDeliver_DistinctIntentsSendDistinctMessagesAndPartsJoin(t *testing.T) {
	f := newDeliveryFixture(t)
	for _, id := range []string{"turn-a", "turn-b"} {
		if _, err := f.h.adapter.Deliver(t.Context(), f.request(id, defaultReply(), "same answer")); err != nil {
			t.Fatalf("Deliver %s: %v", id, err)
		}
	}
	if _, err := f.h.adapter.Deliver(t.Context(), f.request("turn-c", defaultReply(), "first part", "  ", "second part")); err != nil {
		t.Fatalf("Deliver turn-c: %v", err)
	}
	mails := f.server.received()
	if len(mails) != 3 {
		t.Fatalf("server received %d messages, want 3", len(mails))
	}
	ids := map[string]bool{}
	for _, received := range mails {
		ids[parseReceived(t, received).Header.Get("Message-Id")] = true
	}
	if len(ids) != 3 {
		t.Errorf("message ids = %v, want three distinct ids", ids)
	}
	if body := bodyOf(t, parseReceived(t, mails[2])); body != "first part\n\nsecond part\n" {
		t.Errorf("joined body = %q, want one message without splitting", body)
	}
}

func prepareLikeDeliver(t *testing.T, f *deliveryFixture, journal *effect.Journal, req *runtimechannelhost.DeliveryRequest) *effect.Intent {
	t.Helper()
	var target replyReference
	if err := json.Unmarshal(req.ReplyContext, &target); err != nil {
		t.Fatalf("unmarshal reply context: %v", err)
	}
	operation := f.h.adapter.operationFor(&target, keyBase(req), replyBody([]string{req.Parts[0].Text}))
	request, err := json.Marshal(&operation)
	if err != nil {
		t.Fatalf("marshal operation: %v", err)
	}
	intent, err := journal.Prepare(t.Context(), &effect.PrepareRequest{
		SessionID:      req.ConversationID,
		IdempotencyKey: idempotencyKey(keyBase(req), req.ConversationID, &operation),
		Provider:       ProviderName,
		Operation:      OpSendReply,
		Classification: effect.ClassificationEffectful,
		Request:        request,
		EventKind:      effect.EventKindChannelRequested,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return intent
}

func restartedAdapter(t *testing.T, f *deliveryFixture) (*Adapter, *effect.Journal) {
	t.Helper()
	journal, err := effect.NewJournal(f.h.db, effect.Options{Logger: f.h.logger})
	if err != nil {
		t.Fatalf("NewJournal: %v", err)
	}
	executor, err := effect.NewExecutor(journal)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	adapter, err := New(f.h.cfg, f.h.account, f.h.mailbox, f.mailer, f.h.cursors, executor, f.h.sessions, f.h.logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	adapter.now = func() time.Time { return fixedNow }
	return adapter, journal
}

func TestDeliver_RestartBetweenIntentAndSendSendsOnce(t *testing.T) {
	f := newDeliveryFixture(t)
	req := f.request("turn-restart", defaultReply(), "after the restart")
	adapter, journal := restartedAdapter(t, f)
	intent := prepareLikeDeliver(t, f, journal, req)
	if intent.State != effect.StatePrepared || len(f.server.sessions()) != 0 {
		t.Fatalf("fixture state = %s with %d connections, want a prepared intent and no send", intent.State, len(f.server.sessions()))
	}
	for attempt := range 2 {
		if _, err := adapter.Deliver(t.Context(), req); err != nil {
			t.Fatalf("Deliver %d after restart: %v", attempt, err)
		}
	}
	if got := len(f.server.received()); got != 1 {
		t.Fatalf("server received %d messages across the restart, want 1", got)
	}
	f.wantIntents(map[effect.State][]string{effect.StateSucceeded: {""}})
}

func TestDeliver_CrashDuringTheSendIsNeverResent(t *testing.T) {
	cases := map[string]bool{"before recovery": false, "after recovery": true}
	for name, recover := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t)
			req := f.request("turn-crash", defaultReply(), "maybe sent")
			adapter, journal := restartedAdapter(t, f)
			intent := prepareLikeDeliver(t, f, journal, req)
			if _, err := journal.Start(t.Context(), intent.ID); err != nil {
				t.Fatalf("Start: %v", err)
			}
			wantState := effect.StateStarted
			if recover {
				report, err := journal.Recover(t.Context())
				if err != nil || report.Claimed != 1 {
					t.Fatalf("Recover = %+v, %v; want the started intent claimed", report, err)
				}
				wantState = effect.StateUnknown
			}
			_, err := adapter.Deliver(t.Context(), req)
			wantCode(t, err, ErrorCodeDeliveryAmbiguous)
			if len(f.server.sessions()) != 0 {
				t.Error("an intent that may already have been sent was sent again")
			}
			f.wantIntents(map[effect.State][]string{wantState: {""}})
		})
	}
}

func TestDeliver_FailureAfterDataLeavesTheIntentUnknownAndNeverResends(t *testing.T) {
	cases := map[string]smtpBehavior{
		"connection dropped after the body": {afterData: "drop"},
		"no answer after the body":          {afterData: "stall"},
		"temporary failure after the body":  {finalReply: "451 4.3.0 try later"},
	}
	for name, behavior := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t, behavior)
			f.mailer.stepTimeout = shortTimeout
			req := f.request("turn-ambiguous", defaultReply(), "possibly delivered")
			_, err := f.h.adapter.Deliver(t.Context(), req)
			wantCode(t, err, ErrorCodeDeliveryAmbiguous)
			for range 2 {
				_, err = f.h.adapter.Deliver(t.Context(), req)
				wantCode(t, err, ErrorCodeDeliveryAmbiguous)
			}
			if len(f.server.sessions()) != 1 || len(f.server.received()) != 1 {
				t.Fatalf("connections %d, messages %d; want a single submission and no resend", len(f.server.sessions()), len(f.server.received()))
			}
			f.wantIntents(map[effect.State][]string{effect.StateUnknown: {""}})
		})
	}
}

func TestDeliver_PermanentRejectionAfterDataIsFailedNotUnknown(t *testing.T) {
	f := newDeliveryFixture(t, smtpBehavior{finalReply: "554 5.6.0 refused"})
	_, err := f.h.adapter.Deliver(t.Context(), f.request("turn-refused", defaultReply(), "refused"))
	wantCode(t, err, ErrorCodeDeliveryFailed)
	f.wantIntents(map[effect.State][]string{effect.StateFailed: {"email_submission_rejected"}})
	if len(f.server.sessions()) != 1 {
		t.Errorf("connections = %d, want 1", len(f.server.sessions()))
	}
}

func TestDeliver_FailureBeforeDataRetriesWithinTheBound(t *testing.T) {
	f := newDeliveryFixture(t, smtpBehavior{refuseGreeting: true}, smtpBehavior{rcptReply: "451 4.2.0 busy"})
	if _, err := f.h.adapter.Deliver(t.Context(), f.request("turn-retry", defaultReply(), "eventually")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(f.server.sessions()) != 3 || len(f.server.received()) != 1 {
		t.Fatalf("connections %d, messages %d; want 3 connections and one message", len(f.server.sessions()), len(f.server.received()))
	}
	f.wantIntents(map[effect.State][]string{effect.StateSucceeded: {""}})
}

func TestDeliver_RetriesStopAtTheAttemptBoundAndTheIntentFails(t *testing.T) {
	refuse := smtpBehavior{refuseGreeting: true}
	f := newDeliveryFixture(t, refuse, refuse, refuse, refuse, refuse)
	req := f.request("turn-down", defaultReply(), "unreachable")
	_, err := f.h.adapter.Deliver(t.Context(), req)
	wantCode(t, err, ErrorCodeDeliveryFailed)
	if got := len(f.server.sessions()); got != sendMaxAttempts {
		t.Fatalf("connections = %d, want %d attempts", got, sendMaxAttempts)
	}
	f.wantIntents(map[effect.State][]string{effect.StateFailed: {"email_connection_failed"}})
	_, err = f.h.adapter.Deliver(t.Context(), req)
	wantCode(t, err, ErrorCodeDeliveryFailed)
	if got := len(f.server.sessions()); got != sendMaxAttempts {
		t.Errorf("redelivery of a failed intent connected again (%d connections)", got)
	}
	if len(f.server.received()) != 0 {
		t.Error("a message was delivered")
	}
}

func TestDeliver_PermanentFailuresDoNotRetryAndSurfaceInHealth(t *testing.T) {
	cases := map[string]struct {
		behavior smtpBehavior
		mutate   func(*deliveryFixture)
		intent   string
		finding  string
	}{
		"credentials rejected": {behavior: smtpBehavior{authReply: "535 5.7.8 no"}, intent: "email_login_rejected", finding: faultSMTPLogin},
		"no starttls":          {behavior: smtpBehavior{noStartTLS: true, authBeforeTLS: true}, intent: "email_tls_required", finding: faultSMTPTLS},
		"credentials deferred": {behavior: smtpBehavior{authReply: "454 4.7.0 later"}, intent: "email_login_rejected", finding: faultSMTPLogin},
		"starttls deferred":    {behavior: smtpBehavior{startTLSReply: "454 4.7.0 later"}, intent: "email_tls_required", finding: faultSMTPTLS},
		"sender refused":       {behavior: smtpBehavior{mailReply: "550 5.7.1 no"}, intent: "email_submission_rejected"},
		"password unavailable": {mutate: func(f *deliveryFixture) { f.t.Setenv(smtpPasswordEnv, "") }, intent: "email_credentials_unavailable", finding: faultSMTPUnresolved},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t, tc.behavior)
			if tc.mutate != nil {
				tc.mutate(f)
			}
			_, err := f.h.adapter.Deliver(t.Context(), f.request("turn-permanent", defaultReply(), "refused"))
			wantCode(t, err, ErrorCodeDeliveryFailed)
			if got := len(f.server.sessions()); got > 1 {
				t.Errorf("connections = %d, want no retry of a permanent failure", got)
			}
			f.wantIntents(map[effect.State][]string{effect.StateFailed: {tc.intent}})
			var codes []string
			for _, finding := range f.h.adapter.Check(t.Context()) {
				codes = append(codes, finding.Code)
			}
			if tc.finding == "" && len(codes) != 0 || tc.finding != "" && fmt.Sprint(codes) != fmt.Sprint([]string{tc.finding}) {
				t.Errorf("health findings = %v, want %q", codes, tc.finding)
			}
		})
	}
}

func TestDeliver_UntrustedCertificateFailsWithoutSendingCredentials(t *testing.T) {
	authority := newAuthority(t)
	stranger := newAuthority(t)
	server := newFakeSMTP(t, authority)
	certificate := stranger.issue(t, nil, []string{"localhost"})
	server.script(smtpBehavior{certificate: &certificate})
	mailer := newTestMailer(t, server, authority)
	h := newHarness(t, harnessOptions{mailer: mailer})
	f := &deliveryFixture{t: t, h: h, server: server, authority: authority, mailer: mailer}
	_, err := h.adapter.Deliver(t.Context(), f.request("turn-cert", defaultReply(), "never sent"))
	wantCode(t, err, ErrorCodeDeliveryFailed)
	f.wantIntents(map[effect.State][]string{effect.StateFailed: {"email_tls_untrusted"}})
	if got := len(server.sessions()); got != 1 {
		t.Errorf("connections = %d, want no retry", got)
	}
	for _, session := range server.sessions() {
		if session.authenticated || slices.Contains(commandVerbs(&session), "AUTH") {
			t.Error("credentials were offered to an unverified server")
		}
	}
	findings := h.adapter.Check(t.Context())
	if len(findings) != 1 || findings[0].Code != faultSMTPTLS {
		t.Errorf("findings = %+v, want the unsafe TLS finding", findings)
	}
}

func TestDeliver_HealthRecoversAfterASuccessfulSend(t *testing.T) {
	f := newDeliveryFixture(t, smtpBehavior{authReply: "535 5.7.8 no"})
	if _, err := f.h.adapter.Deliver(t.Context(), f.request("turn-bad", defaultReply(), "first")); err == nil {
		t.Fatal("Deliver succeeded with rejected credentials")
	}
	if got := f.h.adapter.Check(t.Context()); len(got) != 1 || got[0].Code != faultSMTPLogin {
		t.Fatalf("findings = %+v, want the login finding", got)
	}
	if _, err := f.h.adapter.Deliver(t.Context(), f.request("turn-good", defaultReply(), "second")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if got := f.h.adapter.Check(t.Context()); len(got) != 0 {
		t.Errorf("findings = %+v, want none after a successful send", got)
	}
}

func TestDeliver_HealthNeverConnectsToSMTP(t *testing.T) {
	f := newDeliveryFixture(t)
	f.h.adapter.Check(t.Context())
	f.h.adapter.Health(t.Context())
	if got := len(f.server.sessions()); got != 0 {
		t.Errorf("health probes opened %d submission connections", got)
	}
}

func TestDeliver_FailsClosedWithoutAUsableRecipient(t *testing.T) {
	cases := map[string]func(t *testing.T, req *runtimechannelhost.DeliveryRequest){
		"no reply context": func(t *testing.T, req *runtimechannelhost.DeliveryRequest) { req.ReplyContext = nil },
		"empty object":     func(t *testing.T, req *runtimechannelhost.DeliveryRequest) { req.ReplyContext = json.RawMessage(`{}`) },
		"not json": func(t *testing.T, req *runtimechannelhost.DeliveryRequest) {
			req.ReplyContext = json.RawMessage(`<owner@example.com>`)
		},
		"message id only": func(t *testing.T, req *runtimechannelhost.DeliveryRequest) {
			req.ReplyContext = json.RawMessage(`{"message_id":"m1@example.com"}`)
		},
		"own address": func(t *testing.T, req *runtimechannelhost.DeliveryRequest) {
			req.ReplyContext = replyContextFor(t, testAddress)
		},
		"line break in address": func(t *testing.T, req *runtimechannelhost.DeliveryRequest) {
			req.ReplyContext = replyContextFor(t, "owner@example.com\r\nBcc: x@evil.example")
		},
		"angle bracket": func(t *testing.T, req *runtimechannelhost.DeliveryRequest) {
			req.ReplyContext = replyContextFor(t, "a>b@example.com")
		},
		"address list": func(t *testing.T, req *runtimechannelhost.DeliveryRequest) {
			req.ReplyContext = replyContextFor(t, "a@example.com,b@example.com")
		},
		"uppercase": func(t *testing.T, req *runtimechannelhost.DeliveryRequest) {
			req.ReplyContext = replyContextFor(t, "Owner@example.com")
		},
		"no text": func(t *testing.T, req *runtimechannelhost.DeliveryRequest) {
			req.Parts = []runtimechannelhost.OutputPart{{Text: " \n"}}
		},
		"no channel":      func(t *testing.T, req *runtimechannelhost.DeliveryRequest) { req.Channel = "" },
		"no conversation": func(t *testing.T, req *runtimechannelhost.DeliveryRequest) { req.ConversationID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t)
			req := f.request("turn-closed", defaultReply(), "answer")
			mutate(t, req)
			_, err := f.h.adapter.Deliver(t.Context(), req)
			wantCode(t, err, ErrorCodeInvalidArgument)
			if len(f.server.sessions()) != 0 {
				t.Error("the adapter connected without a usable recipient")
			}
			f.wantIntents(map[effect.State][]string{})
		})
	}
	f := newDeliveryFixture(t)
	_, err := f.h.adapter.Deliver(t.Context(), nil)
	wantCode(t, err, ErrorCodeInvalidArgument)
}

func replyContextFor(t *testing.T, sender string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(&replyReference{MessageID: "m1@example.com", Sender: sender, Subject: "s"})
	if err != nil {
		t.Fatalf("marshal reply context: %v", err)
	}
	return raw
}

func TestDeliver_WireHeadersStayOnOneLineAndEncodeNonASCII(t *testing.T) {
	cases := map[string]struct {
		subject string
		want    string
	}{
		"crlf with a bcc line": {subject: "hello\r\nBcc: attacker@evil.example\r\nX-Injected: yes", want: "Re: hello Bcc: attacker@evil.example X-Injected: yes"},
		"non-ascii":            {subject: "Rapat énergie 日本語", want: "Re: Rapat énergie 日本語"},
		"stacked replies":      {subject: "Re: RE: Plan the week", want: "Re: Plan the week"},
		"empty subject":        {subject: "", want: "Re:"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t)
			reply := defaultReply()
			reply.Subject = tc.subject
			reply.References = []string{"ok@example.com", "evil@example.com>\r\nBcc: attacker@evil.example"}
			reply.MessageID = "m1@example.com"
			if _, err := f.h.adapter.Deliver(t.Context(), f.request("turn-"+name, reply, "body")); err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			mails := f.server.received()
			if len(mails) != 1 {
				t.Fatalf("messages = %d, want 1", len(mails))
			}
			parsed := parseReceived(t, mails[0])
			for _, header := range []string{"Bcc", "X-Injected", "Cc"} {
				if _, present := parsed.Header[header]; present {
					t.Errorf("header %s reached the wire", header)
				}
			}
			headerBlock, _, _ := strings.Cut(mails[0].data, "\r\n\r\n")
			if strings.IndexFunc(headerBlock, func(r rune) bool { return r > 0x7e }) >= 0 {
				t.Error("a header carries non-ASCII bytes")
			}
			decoded, err := new(mimepkg.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
			if err != nil || decoded != tc.want {
				t.Errorf("Subject = %q (%v), want %q", decoded, err, tc.want)
			}
			if len(mails[0].to) != 1 {
				t.Errorf("recipients = %v, want one", mails[0].to)
			}
		})
	}
}

func TestDeliver_BoundsTheBodyAndKeepsItDecodable(t *testing.T) {
	f := newDeliveryFixture(t)
	huge := strings.Repeat("日本語のテキスト。", MaxReplyBodyBytes)
	if _, err := f.h.adapter.Deliver(t.Context(), f.request("turn-huge", defaultReply(), huge)); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	parsed := parseReceived(t, f.server.received()[0])
	var body bytes.Buffer
	decoded := decodedBody(t, parsed)
	body.WriteString(decoded)
	if body.Len() > MaxReplyBodyBytes+2 || !strings.Contains(decoded, "[reply truncated]") {
		t.Errorf("decoded body is %d bytes, want at most %d and a truncation notice", body.Len(), MaxReplyBodyBytes)
	}
}

func TestDeliver_ErrorsAndLogsCarryNoAddressesSubjectsBodiesOrSecrets(t *testing.T) {
	const subject, body = "TOPSECRETSUBJECT", "TOPSECRETBODY"
	cases := map[string]smtpBehavior{
		"credentials rejected": {authReply: "535 5.7.8 owner@example.com rejected"},
		"recipient refused":    {rcptReply: "550 5.1.1 owner@example.com TOPSECRETBODY unknown"},
		"dropped after body":   {afterData: "drop"},
		"refused after body":   {finalReply: "554 5.6.0 owner@example.com TOPSECRETSUBJECT"},
	}
	for name, behavior := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t, behavior)
			reply := defaultReply()
			reply.Subject = subject
			_, err := f.h.adapter.Deliver(t.Context(), f.request("turn-leak", reply, body))
			if err == nil {
				t.Fatal("Deliver succeeded")
			}
			surfaces := []string{err.Error(), f.h.logs.String()}
			for _, check := range f.h.adapter.Check(t.Context()) {
				surfaces = append(surfaces, check.Detail, check.Code)
			}
			for _, surface := range surfaces {
				for _, secret := range []string{ownerAddress, testAddress, subject, body, smtpPassword, "m1@example.com", "example.com"} {
					if strings.Contains(surface, secret) {
						t.Errorf("%q surfaces %q", surface, secret)
					}
				}
			}
		})
	}
}

func TestInvoke_RejectsMalformedRequestsWithoutSubmitting(t *testing.T) {
	mailer := &fakeMailer{}
	h := newHarness(t, harnessOptions{mailer: mailer})
	_, err := h.adapter.Invoke(t.Context(), nil)
	wantCode(t, err, ErrorCodeInvalidArgument)
	_, err = h.adapter.Invoke(t.Context(), &effect.Invocation{Operation: "edit_reply", Request: json.RawMessage(`{}`)})
	wantCode(t, err, ErrorCodeInvalidArgument)
	_, err = h.adapter.Invoke(t.Context(), &effect.Invocation{Operation: OpSendReply, Request: json.RawMessage(`[`)})
	wantCode(t, err, ErrorCodeInvalidArgument)
	cases := map[string]sendOperation{
		"own address":       {To: testAddress, MessageID: "id@example.com", Body: "text"},
		"unsafe recipient":  {To: "a>b@example.com", MessageID: "id@example.com", Body: "text"},
		"unsafe message id": {To: ownerAddress, MessageID: "id @example.com", Body: "text"},
		"empty body":        {To: ownerAddress, MessageID: "id@example.com", Body: " "},
	}
	for name, operation := range cases {
		t.Run(name, func(t *testing.T) {
			raw, marshalErr := json.Marshal(&operation)
			if marshalErr != nil {
				t.Fatalf("marshal: %v", marshalErr)
			}
			outcome, err := h.adapter.Invoke(t.Context(), &effect.Invocation{Operation: OpSendReply, Request: raw})
			if err != nil || outcome.Succeeded || outcome.Ambiguous || outcome.SafeErrorCode != "email_message_invalid" {
				t.Fatalf("outcome = %+v, %v; want a failed outcome with %s", outcome, err, "email_message_invalid")
			}
		})
	}
	if got := len(mailer.sent()); got != 0 {
		t.Errorf("malformed requests produced %d submissions", got)
	}
	if h.adapter.SupportsIdempotency() {
		t.Error("SMTP submission must not claim idempotency")
	}
}

func TestSubmit_BacksOffBetweenAttemptsAndBoundsThem(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mailer := &fakeMailer{}
		h := newHarness(t, harnessOptions{mailer: mailer})
		h.adapter.retryBase = time.Second
		h.adapter.retryCap = time.Minute
		submission := &Submission{From: testAddress, To: ownerAddress, Message: []byte("x")}
		down := Errorf(ErrorCodeConnectionFailed, "down")

		mailer.fail(down, down)
		start := time.Now()
		outcome, err := h.adapter.submit(t.Context(), submission, "id@example.com")
		if err != nil || !outcome.Succeeded {
			t.Fatalf("outcome = %+v, %v; want success on the third attempt", outcome, err)
		}
		if got := time.Since(start); got != 3*time.Second {
			t.Errorf("waited %v, want 1s then 2s", got)
		}
		if got := len(mailer.sent()); got != 3 {
			t.Errorf("submissions = %d, want 3", got)
		}

		mailer.fail(down, down, down, down)
		start = time.Now()
		before := len(mailer.sent())
		outcome, err = h.adapter.submit(t.Context(), submission, "id@example.com")
		if err != nil || outcome.Succeeded || outcome.Ambiguous || outcome.SafeErrorCode != "email_connection_failed" {
			t.Fatalf("outcome = %+v, %v; want a failed outcome after the attempt bound", outcome, err)
		}
		if got := len(mailer.sent()) - before; got != sendMaxAttempts {
			t.Errorf("attempts = %d, want %d", got, sendMaxAttempts)
		}
		if got := time.Since(start); got != 3*time.Second {
			t.Errorf("waited %v, want no wait after the last attempt", got)
		}
	})
}

func TestSubmit_OutcomeClassification(t *testing.T) {
	cases := map[string]struct {
		failure   error
		attempts  int
		ambiguous bool
		code      string
	}{
		"ambiguous after data":    {failure: Errorf(ErrorCodeDeliveryAmbiguous, "x"), attempts: 1, ambiguous: true},
		"uncoded error":           {failure: errors.New("boom"), attempts: 1, ambiguous: true},
		"deferred is retried":     {failure: Errorf(ErrorCodeSubmissionDeferred, "x"), attempts: sendMaxAttempts, code: "email_submission_deferred"},
		"rejected is not retried": {failure: Errorf(ErrorCodeSubmissionRejected, "x"), attempts: 1, code: "email_submission_rejected"},
		"login is not retried":    {failure: Errorf(ErrorCodeLoginRejected, "x"), attempts: 1, code: "email_login_rejected"},
		"tls is not retried":      {failure: Errorf(ErrorCodeTLSUntrusted, "x"), attempts: 1, code: "email_tls_untrusted"},
		"password is not retried": {failure: Errorf(ErrorCodeCredentialsUnavailable, "x"), attempts: 1, code: "email_credentials_unavailable"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				mailer := &fakeMailer{}
				mailer.fail(tc.failure, tc.failure, tc.failure)
				h := newHarness(t, harnessOptions{mailer: mailer})
				outcome, err := h.adapter.submit(t.Context(), &Submission{From: testAddress, To: ownerAddress, Message: []byte("x")}, "id@example.com")
				if err != nil || outcome.Succeeded || outcome.Ambiguous != tc.ambiguous || outcome.SafeErrorCode != tc.code {
					t.Fatalf("outcome = %+v, %v; want ambiguous=%v code=%q", outcome, err, tc.ambiguous, tc.code)
				}
				if got := len(mailer.sent()); got != tc.attempts {
					t.Errorf("attempts = %d, want %d", got, tc.attempts)
				}
			})
		})
	}
}

func TestSubmit_CancellationDuringBackoffFailsWithoutAmbiguity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mailer := &fakeMailer{}
		mailer.fail(Errorf(ErrorCodeConnectionFailed, "down"))
		h := newHarness(t, harnessOptions{mailer: mailer})
		h.adapter.retryBase = time.Hour
		h.adapter.retryCap = time.Hour
		ctx, cancel := context.WithCancel(t.Context())
		type result struct {
			outcome effect.Outcome
			err     error
		}
		done := make(chan result, 1)
		go func() {
			outcome, err := h.adapter.submit(ctx, &Submission{From: testAddress, To: ownerAddress, Message: []byte("x")}, "id@example.com")
			done <- result{outcome, err}
		}()
		synctest.Wait()
		cancel()
		got := <-done
		if got.err != nil || got.outcome.Ambiguous || got.outcome.Succeeded || got.outcome.SafeErrorCode != faultCancelled {
			t.Fatalf("outcome = %+v, %v; want a failed outcome with %s", got.outcome, got.err, faultCancelled)
		}
		if n := len(mailer.sent()); n != 1 {
			t.Errorf("attempts = %d, want 1", n)
		}
	})
}

func TestDeliver_HandshakeFailuresFailOnceAsUnsafeTLS(t *testing.T) {
	cases := map[string]smtpBehavior{
		"alert":   {handshake: "alert"},
		"garbage": {handshake: "garbage"},
		"stall":   {handshake: "stall"},
	}
	for name, behavior := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDeliveryFixture(t, behavior, behavior, behavior)
			f.mailer.stepTimeout = shortTimeout
			_, err := f.h.adapter.Deliver(t.Context(), f.request("turn-handshake", defaultReply(), "never sent"))
			wantCode(t, err, ErrorCodeDeliveryFailed)
			if got := len(f.server.sessions()); got != 1 {
				t.Fatalf("connections = %d, want exactly 1", got)
			}
			f.wantIntents(map[effect.State][]string{effect.StateFailed: {"email_tls_required"}})
			findings := f.h.adapter.Check(t.Context())
			if len(findings) != 1 || findings[0].Code != faultSMTPTLS {
				t.Errorf("findings = %+v, want %s", findings, faultSMTPTLS)
			}
		})
	}
}

func TestDeliver_TemporaryAuthFailureFailsOnce(t *testing.T) {
	behavior := smtpBehavior{authReply: "454 4.7.0 try later"}
	f := newDeliveryFixture(t, behavior, behavior, behavior)
	_, err := f.h.adapter.Deliver(t.Context(), f.request("turn-auth-4xx", defaultReply(), "never sent"))
	wantCode(t, err, ErrorCodeDeliveryFailed)
	if got := len(f.server.sessions()); got != 1 {
		t.Fatalf("connections = %d, want exactly 1", got)
	}
	f.wantIntents(map[effect.State][]string{effect.StateFailed: {"email_login_rejected"}})
	findings := f.h.adapter.Check(t.Context())
	if len(findings) != 1 || findings[0].Code != faultSMTPLogin {
		t.Errorf("findings = %+v, want %s", findings, faultSMTPLogin)
	}
}

package email

import (
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/effect"
	"github.com/anggasct/aura/internal/store"
)

const (
	recipientAddr = "customer@example.net"
	sendSession   = "main/email:work/thread:send"
)

func (f *deliveryFixture) outgoing(key string) *Outgoing {
	f.t.Helper()
	if err := (&storeSessions{sessions: store.NewSessionService(f.h.db)}).EnsureSession(f.t.Context(), sendSession, "local-owner", testPersona); err != nil {
		f.t.Fatalf("ensure session: %v", err)
	}
	return &Outgoing{SessionID: sendSession, IdempotencyKey: key, To: recipientAddr, Subject: "Your invoice", Body: "Attached is the summary."}
}

func TestSend_SubmitsAFreshMessageThroughTheJournal(t *testing.T) {
	f := newDeliveryFixture(t)
	outgoing := f.outgoing("call-1")
	id, err := f.h.adapter.Send(t.Context(), outgoing)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	mails := f.server.received()
	if len(mails) != 1 || len(mails[0].to) != 1 || mails[0].to[0] != "TO:<"+recipientAddr+">" {
		t.Fatalf("received = %+v, want one message to the recipient alone", mails)
	}
	parsed := parseReceived(t, mails[0])
	want := map[string]string{
		"From":           testAddress,
		"To":             recipientAddr,
		"Subject":        "Your invoice",
		"Auto-Submitted": "auto-generated",
	}
	for key, value := range want {
		if got := parsed.Header.Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	for _, header := range []string{"In-Reply-To", "References", "Cc", "Bcc", "Reply-To"} {
		if _, present := parsed.Header[header]; present {
			t.Errorf("header %s present on a fresh message", header)
		}
	}
	if got := parsed.Header.Get("Message-Id"); got != "<"+id+">" {
		t.Errorf("Message-Id = %q, want the returned id %q", got, id)
	}
	if body := bodyOf(t, parsed); !strings.Contains(body, "Attached is the summary.") {
		t.Errorf("body = %q", body)
	}
	f.wantIntents(map[effect.State][]string{effect.StateSucceeded: {""}})
	again, err := f.h.adapter.Send(t.Context(), outgoing)
	if err != nil || again != id || len(f.server.received()) != 1 {
		t.Fatalf("replay = %q, %v with %d messages, want the recorded id and no second submission", again, err, len(f.server.received()))
	}
}

func TestSend_DistinctCallsAndRecipientsAreDistinctIntents(t *testing.T) {
	f := newDeliveryFixture(t)
	first := f.outgoing("call-a")
	second := *first
	second.IdempotencyKey = "call-b"
	third := *first
	third.IdempotencyKey = "call-a"
	third.To = "other@example.net"
	for _, outgoing := range []*Outgoing{first, &second, &third} {
		if _, err := f.h.adapter.Send(t.Context(), outgoing); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if got := len(f.server.received()); got != 3 {
		t.Fatalf("messages = %d, want three distinct submissions", got)
	}
}

func TestSend_RefusesUnusableOutgoingMail(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Outgoing)
		code   ErrorCode
	}{
		{name: "empty recipient", mutate: func(o *Outgoing) { o.To = "" }, code: ErrorCodeMessageInvalid},
		{name: "own address", mutate: func(o *Outgoing) { o.To = testAddress }, code: ErrorCodeMessageInvalid},
		{name: "display name form", mutate: func(o *Outgoing) { o.To = "Bob <bob@example.net>" }, code: ErrorCodeMessageInvalid},
		{name: "two recipients", mutate: func(o *Outgoing) { o.To = "a@example.net,b@example.net" }, code: ErrorCodeMessageInvalid},
		{name: "header injection in the recipient", mutate: func(o *Outgoing) { o.To = "a@example.net\r\nBcc: x@evil.example" }, code: ErrorCodeMessageInvalid},
		{name: "uppercase recipient", mutate: func(o *Outgoing) { o.To = "Bob@example.net" }, code: ErrorCodeMessageInvalid},
		{name: "empty body", mutate: func(o *Outgoing) { o.Body = " \n" }, code: ErrorCodeMessageInvalid},
		{name: "oversized body", mutate: func(o *Outgoing) { o.Body = strings.Repeat("x", MaxReplyBodyBytes+1) }, code: ErrorCodeMessageInvalid},
		{name: "missing session", mutate: func(o *Outgoing) { o.SessionID = "" }, code: ErrorCodeInvalidArgument},
		{name: "missing idempotency key", mutate: func(o *Outgoing) { o.IdempotencyKey = "" }, code: ErrorCodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliveryFixture(t)
			outgoing := f.outgoing("call-bad")
			tc.mutate(outgoing)
			_, err := f.h.adapter.Send(t.Context(), outgoing)
			wantCode(t, err, tc.code)
			if len(f.server.sessions()) != 0 {
				t.Error("the adapter connected for an unusable message")
			}
			f.wantIntents(map[effect.State][]string{})
		})
	}
	f := newDeliveryFixture(t)
	_, err := f.h.adapter.Send(t.Context(), nil)
	wantCode(t, err, ErrorCodeInvalidArgument)
}

func TestSend_SubjectsAreHeaderSafe(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		want    string
	}{
		{name: "plain", subject: "Hello", want: "Hello"},
		{name: "empty falls back", subject: "", want: noSubject},
		{name: "line breaks are folded away", subject: "Hello\r\nBcc: x@evil.example", want: "Hello Bcc: x@evil.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliveryFixture(t)
			outgoing := f.outgoing("call-subject")
			outgoing.Subject = tc.subject
			if _, err := f.h.adapter.Send(t.Context(), outgoing); err != nil {
				t.Fatalf("Send: %v", err)
			}
			mails := f.server.received()
			if len(mails) != 1 {
				t.Fatalf("messages = %d, want 1", len(mails))
			}
			parsed := parseReceived(t, mails[0])
			if got := parsed.Header.Get("Subject"); got != tc.want {
				t.Errorf("Subject = %q, want %q", got, tc.want)
			}
			if _, present := parsed.Header["Bcc"]; present {
				t.Error("a subject smuggled a header")
			}
		})
	}
}

func TestSend_AmbiguousOutcomeIsNeverResent(t *testing.T) {
	f := newDeliveryFixture(t, smtpBehavior{afterData: "drop"})
	f.mailer.stepTimeout = shortTimeout
	outgoing := f.outgoing("call-ambiguous")
	for range 2 {
		_, err := f.h.adapter.Send(t.Context(), outgoing)
		wantCode(t, err, ErrorCodeDeliveryAmbiguous)
	}
	if len(f.server.received()) != 1 {
		t.Fatalf("messages = %d, want a single submission and no resend", len(f.server.received()))
	}
	f.wantIntents(map[effect.State][]string{effect.StateUnknown: {""}})
}

func TestDeliverText(t *testing.T) {
	cases := []struct {
		name  string
		reply TextReply
		code  ErrorCode
		check func(t *testing.T, mail receivedMail)
	}{
		{name: "threads onto the last inbound message", reply: TextReply{To: recipientAddr, Thread: ThreadReference{MessageID: "m1@example.net", Subject: "Re: Refund"}, Text: "Owner here."}, check: func(t *testing.T, mail receivedMail) {
			parsed := parseReceived(t, mail)
			if parsed.Header.Get("In-Reply-To") != "<m1@example.net>" || parsed.Header.Get("Subject") != "Re: Refund" || parsed.Header.Get("To") != recipientAddr {
				t.Errorf("headers = %v", parsed.Header)
			}
		}},
		{name: "unsafe thread id is dropped", reply: TextReply{To: recipientAddr, Thread: ThreadReference{MessageID: "m1@example.net>\r\nBcc: x", Subject: "Re: Refund"}, Text: "Owner here."}, check: func(t *testing.T, mail receivedMail) {
			parsed := parseReceived(t, mail)
			if _, present := parsed.Header["In-Reply-To"]; present {
				t.Error("an unsafe message id was threaded")
			}
		}},
		{name: "refuses its own address", reply: TextReply{To: testAddress, Text: "x"}, code: ErrorCodeMessageInvalid},
		{name: "refuses an address list", reply: TextReply{To: "a@example.net,b@example.net", Text: "x"}, code: ErrorCodeMessageInvalid},
		{name: "refuses an empty text", reply: TextReply{To: recipientAddr, Text: " "}, code: ErrorCodeMessageInvalid},
		{name: "refuses an oversized text", reply: TextReply{To: recipientAddr, Text: strings.Repeat("x", MaxReplyBodyBytes+1)}, code: ErrorCodeMessageInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newDeliveryFixture(t)
			err := deliverText(t.Context(), f.mailer, testAddress, &tc.reply, func() time.Time { return fixedNow })
			if tc.code != "" {
				wantCode(t, err, tc.code)
				if len(f.server.sessions()) != 0 {
					t.Error("connected for an unusable reply")
				}
				return
			}
			if err != nil {
				t.Fatalf("deliverText: %v", err)
			}
			mails := f.server.received()
			if len(mails) != 1 {
				t.Fatalf("messages = %d, want 1", len(mails))
			}
			tc.check(t, mails[0])
			f.wantIntents(map[effect.State][]string{})
		})
	}
	if err := DeliverText(t.Context(), nil, &TextReply{}); err == nil {
		t.Error("DeliverText accepted a nil account")
	}
}

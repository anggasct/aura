package email

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

type fakeSendOutbox struct {
	routes map[string]Route
	sent   []Outgoing
	err    error
}

func (o *fakeSendOutbox) RouteOf(sessionID string) (Route, bool) {
	route, ok := o.routes[sessionID]
	return route, ok
}

func (o *fakeSendOutbox) Send(_ context.Context, outgoing *Outgoing) (string, error) {
	o.sent = append(o.sent, *outgoing)
	return "id-1@example.com", o.err
}

const sendToolSession = "main/email:work/thread:abc"

func sendToolRequest(class principal.Class, session, args string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{
		SessionID: session, PrincipalID: "email:ann@example.net", PrincipalClass: class, AgentID: "main",
		ToolName: SendToolName, ToolVersion: tools.Version, Arguments: json.RawMessage(args), IdempotencyKey: "call-1",
	}
}

func TestEmailSendValidation(t *testing.T) {
	definition := SendToolDefinition()
	if !definition.Effectful || !definition.OptIn || definition.Channel != principal.ChannelEmail {
		t.Fatalf("definition = %+v", definition)
	}
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "normalizes the address", raw: `{"to":" Bob@Example.NET ","subject":"Hi","body":"hello"}`, want: `{"body":"hello","subject":"Hi","to":"bob@example.net"}`},
		{name: "rejects multiple recipients", raw: `{"to":"a@example.net,b@example.net","subject":"Hi","body":"hello"}`, wantErr: true},
		{name: "rejects display name", raw: `{"to":"Bob <bob@example.net>","subject":"Hi","body":"hello"}`, wantErr: true},
		{name: "rejects unknown field", raw: `{"to":"bob@example.net","subject":"Hi","body":"hello","cc":"c@example.net"}`, wantErr: true},
		{name: "rejects empty subject", raw: `{"to":"bob@example.net","subject":"","body":"hello"}`, wantErr: true},
		{name: "rejects empty body", raw: `{"to":"bob@example.net","subject":"Hi","body":""}`, wantErr: true},
		{name: "subject at the bound", raw: `{"to":"bob@example.net","subject":"` + strings.Repeat("s", MaxSubjectBytes) + `","body":"text"}`, want: `{"body":"text","subject":"` + strings.Repeat("s", MaxSubjectBytes) + `","to":"bob@example.net"}`},
		{name: "subject over the bound", raw: `{"to":"bob@example.net","subject":"` + strings.Repeat("s", MaxSubjectBytes+1) + `","body":"text"}`, wantErr: true},
		{name: "body over the bound", raw: `{"to":"bob@example.net","subject":"Hi","body":"` + strings.Repeat("b", MaxBodyBytes+1) + `"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := definition.Validate(json.RawMessage(tc.raw))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate(%s) = %s, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(%s): %v", tc.raw, err)
			}
			if string(got) != tc.want {
				t.Fatalf("Validate(%s) = %s, want %s", tc.raw, got, tc.want)
			}
		})
	}
}

func TestEmailSendCardShowsTheRecipientFirst(t *testing.T) {
	definition := SendToolDefinition()
	args := json.RawMessage(`{"to":"bob@example.net","subject":"Status","body":"All good."}`)
	text, err := definition.ApprovalText(args, nil)
	if err != nil {
		t.Fatalf("ApprovalText: %v", err)
	}
	want := "to: bob@example.net\nsubject: Status\nbody: All good."
	if text != want {
		t.Fatalf("display =\n%s\nwant\n%s", text, want)
	}
}

func TestEmailGuardRequiresApprovalUnlessTheRecipientIsTheSessionSender(t *testing.T) {
	stub := &fakeSendOutbox{routes: map[string]Route{sendToolSession: {Account: "work", Sender: "ann@example.net"}}}
	cases := []struct {
		name    string
		outbox  outbox
		session string
		args    string
		want    bool
		wantErr bool
	}{
		{name: "the authenticated sender needs no approval", outbox: stub, session: sendToolSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`},
		{name: "another recipient needs approval", outbox: stub, session: sendToolSession, args: `{"to":"bob@example.net","subject":"s","body":"b"}`, want: true},
		{name: "a look-alike recipient needs approval", outbox: stub, session: sendToolSession, args: `{"to":"ann@example.net.evil.example","subject":"s","body":"b"}`, want: true},
		{name: "an unknown session needs approval", outbox: stub, session: "main/discord:owner/dm:1", args: `{"to":"ann@example.net","subject":"s","body":"b"}`, want: true},
		{name: "no outbox needs approval", session: sendToolSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, want: true},
		{name: "unreadable arguments fail closed", outbox: stub, session: sendToolSession, args: `{`, want: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newToolProvider(tc.outbox)
			regs := provider.Tools()
			if len(regs) != 1 || regs[0].ApprovalGuard == nil {
				t.Fatalf("provider registrations = %+v, want one registration with ApprovalGuard", regs)
			}
			got, err := regs[0].ApprovalGuard(t.Context(), sendToolRequest(principal.ClassOwner, tc.session, tc.args))
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("guard = %v, %v; want %v (error %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
	t.Run("nil request fails closed", func(t *testing.T) {
		provider := newToolProvider(stub)
		got, err := provider.Tools()[0].ApprovalGuard(t.Context(), nil)
		if !got || err == nil {
			t.Fatalf("guard(nil) = %v, %v; want true with invalid-argument error", got, err)
		}
		if code, ok := fault.CodeOf[toolbroker.ResultClass](err); !ok || code != toolbroker.ResultInvalidArgument {
			t.Fatalf("guard(nil) err = %v, want %s", err, toolbroker.ResultInvalidArgument)
		}
	})
}

func TestEmailGuardIgnoresEveryModelSuppliedFieldExceptTheRecipient(t *testing.T) {
	stub := &fakeSendOutbox{routes: map[string]Route{sendToolSession: {Account: "work", Sender: "ann@example.net"}}}
	provider := newToolProvider(stub)
	request := sendToolRequest(principal.ClassOwner, sendToolSession, `{"to":"bob@example.net","subject":"ann@example.net","body":"ann@example.net"}`)
	request.PrincipalID = "email:bob@example.net"
	if required, err := provider.guard(t.Context(), request); err != nil || !required {
		t.Fatalf("guard = %v, %v; want approval when only the principal or text names the sender", required, err)
	}
}

func TestEmailAdapter(t *testing.T) {
	route := map[string]Route{sendToolSession: {Account: "work", Sender: "ann@example.net"}}
	cases := []struct {
		name     string
		outbox   *fakeSendOutbox
		noOutbox bool
		class    principal.Class
		session  string
		args     string
		wantCode toolbroker.ResultClass
		wantSent int
		wantText string
	}{
		{name: "owner sends", outbox: &fakeSendOutbox{routes: route}, class: principal.ClassOwner, session: sendToolSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultOK, wantSent: 1},
		{name: "trusted sends", outbox: &fakeSendOutbox{routes: route}, class: principal.ClassTrusted, session: sendToolSession, args: `{"to":"bob@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultOK, wantSent: 1},
		{name: "public is denied", outbox: &fakeSendOutbox{routes: route}, class: principal.ClassPublic, session: sendToolSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultToolTierDenied},
		{name: "no outbox is unavailable", noOutbox: true, class: principal.ClassOwner, session: sendToolSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultCapabilityUnavailable},
		{name: "a session without an email account is unavailable", outbox: &fakeSendOutbox{routes: route}, class: principal.ClassOwner, session: "main/discord:owner/dm:1", args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultCapabilityUnavailable},
		{name: "an address list is invalid", outbox: &fakeSendOutbox{routes: route}, class: principal.ClassOwner, session: sendToolSession, args: `{"to":"a@example.net,b@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultInvalidArgument},
		{name: "unreadable arguments are invalid", outbox: &fakeSendOutbox{routes: route}, class: principal.ClassOwner, session: sendToolSession, args: `{`, wantCode: toolbroker.ResultInvalidArgument},
		{name: "a send failure is not echoed", outbox: &fakeSendOutbox{routes: route, err: errors.New("smtp password hunter2 rejected")}, class: principal.ClassOwner, session: sendToolSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultExecutionFailed, wantSent: 1, wantText: "email could not be sent"},
		{name: "invalid message maps to invalid argument", outbox: &fakeSendOutbox{routes: route, err: fault.New(ErrorCodeMessageInvalid, "bad header")}, class: principal.ClassOwner, session: sendToolSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultInvalidArgument, wantSent: 1, wantText: "the message cannot be sent as written"},
		{name: "ambiguous delivery maps to execution failed", outbox: &fakeSendOutbox{routes: route, err: fault.New(ErrorCodeDeliveryAmbiguous, "eof")}, class: principal.ClassOwner, session: sendToolSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultExecutionFailed, wantSent: 1, wantText: "the delivery outcome is unknown and the message was not resent"},
		{name: "no route error maps to capability unavailable", outbox: &fakeSendOutbox{routes: route, err: ErrNoRoute}, class: principal.ClassOwner, session: sendToolSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultCapabilityUnavailable, wantSent: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var box outbox
			if !tc.noOutbox {
				box = tc.outbox
			}
			provider := newToolProvider(box)
			result, err := provider.adapter(t.Context(), sendToolRequest(tc.class, tc.session, tc.args), approval.Constraints{})
			got := result.Class
			if err != nil {
				got, _ = fault.CodeOf[toolbroker.ResultClass](err)
				if strings.Contains(err.Error(), "hunter2") {
					t.Errorf("error %q leaks the transport failure", err)
				}
				if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.wantText)
				}
			}
			if got != tc.wantCode {
				t.Fatalf("class = %q (%v), want %q", got, err, tc.wantCode)
			}
			if tc.outbox != nil && len(tc.outbox.sent) != tc.wantSent {
				t.Fatalf("sent = %d, want %d", len(tc.outbox.sent), tc.wantSent)
			}
			if tc.wantCode == toolbroker.ResultOK {
				sent := tc.outbox.sent[0]
				if sent.SessionID != tc.session || sent.IdempotencyKey != "call-1" || sent.Subject != "s" || sent.Body != "b" {
					t.Errorf("sent = %+v", sent)
				}
				var output map[string]string
				if err := json.Unmarshal(result.Output, &output); err != nil || output["status"] != "sent" || output["account"] != "work" || output["message_id"] != "id-1@example.com" {
					t.Errorf("output = %s, %v", result.Output, err)
				}
			}
		})
	}
}

func TestEmailAdapterPropagatesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stub := &fakeSendOutbox{
		routes: map[string]Route{sendToolSession: {Account: "work", Sender: "ann@example.net"}},
		err:    context.Canceled,
	}
	provider := newToolProvider(stub)
	_, err := provider.adapter(ctx, sendToolRequest(principal.ClassOwner, sendToolSession, `{"to":"ann@example.net","subject":"s","body":"b"}`), approval.Constraints{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNewToolProviderWithoutAdaptersRegistersGuardedUnavailableEmailSend(t *testing.T) {
	broker, err := toolbroker.New(&toolbroker.Options{})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	if err := broker.RegisterProvider(NewToolProvider()); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}
	def, ok := broker.Definition(SendToolName, tools.Version)
	if !ok || def.Channel != principal.ChannelEmail || def.Validator == nil {
		t.Fatalf("definition = %+v, ok = %v", def, ok)
	}
}

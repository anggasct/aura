package toolsbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

type fakeOutbox struct {
	routes map[string]EmailRoute
	sent   []EmailMessage
	err    error
}

func (o *fakeOutbox) RouteOf(sessionID string) (EmailRoute, bool) {
	route, ok := o.routes[sessionID]
	return route, ok
}

func (o *fakeOutbox) Send(_ context.Context, message *EmailMessage) (string, error) {
	o.sent = append(o.sent, *message)
	return "id-1@example.com", o.err
}

const emailSession = "main/email:work/thread:abc"

func emailRequest(class principal.Class, session, args string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{
		SessionID: session, PrincipalID: "email:ann@example.net", PrincipalClass: class, AgentID: "main",
		ToolName: tools.EmailSendToolName, ToolVersion: tools.Version, Arguments: json.RawMessage(args), IdempotencyKey: "call-1",
	}
}

func newEmailExecutor(outbox EmailOutbox) (*Executor, *emailHolder) {
	holder := &emailHolder{}
	executor := &Executor{emails: holder}
	if outbox != nil {
		executor.SetEmailOutbox(outbox)
	}
	return executor, holder
}

func TestEmailGuardRequiresApprovalUnlessTheRecipientIsTheSessionSender(t *testing.T) {
	outbox := &fakeOutbox{routes: map[string]EmailRoute{emailSession: {Account: "work", Sender: "ann@example.net"}}}
	cases := []struct {
		name    string
		outbox  EmailOutbox
		session string
		args    string
		want    bool
		wantErr bool
	}{
		{name: "the authenticated sender needs no approval", outbox: outbox, session: emailSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`},
		{name: "another recipient needs approval", outbox: outbox, session: emailSession, args: `{"to":"bob@example.net","subject":"s","body":"b"}`, want: true},
		{name: "a look-alike recipient needs approval", outbox: outbox, session: emailSession, args: `{"to":"ann@example.net.evil.example","subject":"s","body":"b"}`, want: true},
		{name: "an unknown session needs approval", outbox: outbox, session: "main/discord:owner/dm:1", args: `{"to":"ann@example.net","subject":"s","body":"b"}`, want: true},
		{name: "no outbox needs approval", session: emailSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, want: true},
		{name: "unreadable arguments fail", outbox: outbox, session: emailSession, args: `{`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, holder := newEmailExecutor(tc.outbox)
			got, err := holder.guard(t.Context(), emailRequest(principal.ClassOwner, tc.session, tc.args))
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("guard = %v, %v; want %v (error %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestEmailGuardIgnoresEveryModelSuppliedFieldExceptTheRecipient(t *testing.T) {
	outbox := &fakeOutbox{routes: map[string]EmailRoute{emailSession: {Account: "work", Sender: "ann@example.net"}}}
	_, holder := newEmailExecutor(outbox)
	request := emailRequest(principal.ClassOwner, emailSession, `{"to":"bob@example.net","subject":"ann@example.net","body":"ann@example.net"}`)
	request.PrincipalID = "email:bob@example.net"
	if required, err := holder.guard(t.Context(), request); err != nil || !required {
		t.Fatalf("guard = %v, %v; want approval when only the principal or text names the sender", required, err)
	}
}

func TestEmailAdapter(t *testing.T) {
	route := map[string]EmailRoute{emailSession: {Account: "work", Sender: "ann@example.net"}}
	cases := []struct {
		name     string
		outbox   *fakeOutbox
		noOutbox bool
		class    principal.Class
		session  string
		args     string
		wantCode toolbroker.ResultClass
		wantSent int
	}{
		{name: "owner sends", outbox: &fakeOutbox{routes: route}, class: principal.ClassOwner, session: emailSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultOK, wantSent: 1},
		{name: "trusted sends", outbox: &fakeOutbox{routes: route}, class: principal.ClassTrusted, session: emailSession, args: `{"to":"bob@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultOK, wantSent: 1},
		{name: "public is denied", outbox: &fakeOutbox{routes: route}, class: principal.ClassPublic, session: emailSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultToolTierDenied},
		{name: "no outbox is unavailable", noOutbox: true, class: principal.ClassOwner, session: emailSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultCapabilityUnavailable},
		{name: "a session without an email account is unavailable", outbox: &fakeOutbox{routes: route}, class: principal.ClassOwner, session: "main/discord:owner/dm:1", args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultCapabilityUnavailable},
		{name: "an address list is invalid", outbox: &fakeOutbox{routes: route}, class: principal.ClassOwner, session: emailSession, args: `{"to":"a@example.net,b@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultInvalidArgument},
		{name: "unreadable arguments are invalid", outbox: &fakeOutbox{routes: route}, class: principal.ClassOwner, session: emailSession, args: `{`, wantCode: toolbroker.ResultInvalidArgument},
		{name: "a send failure is not echoed", outbox: &fakeOutbox{routes: route, err: errors.New("smtp password hunter2 rejected")}, class: principal.ClassOwner, session: emailSession, args: `{"to":"ann@example.net","subject":"s","body":"b"}`, wantCode: toolbroker.ResultExecutionFailed, wantSent: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var outbox EmailOutbox
			if !tc.noOutbox {
				outbox = tc.outbox
			}
			_, holder := newEmailExecutor(outbox)
			result, err := holder.adapter(t.Context(), emailRequest(tc.class, tc.session, tc.args), approval.Constraints{})
			got := result.Class
			if err != nil {
				got, _ = toolbroker.CodeOf(err)
				if strings.Contains(err.Error(), "hunter2") {
					t.Errorf("error %q leaks the transport failure", err)
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

func TestSetEmailOutboxIgnoresNilReceiversAndOutboxes(t *testing.T) {
	var absent *Executor
	absent.SetEmailOutbox(&fakeOutbox{})
	executor, holder := newEmailExecutor(nil)
	executor.SetEmailOutbox(nil)
	if _, _, ok := holder.route(emailSession); ok {
		t.Error("a nil outbox resolved a route")
	}
}

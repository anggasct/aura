package httpchat

import (
	"context"
	"errors"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/channel/approval"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

type ownerCall struct {
	prompt    approval.Prompt
	requester approval.Requester
}

type ownerStub struct {
	mu     sync.Mutex
	calls  []ownerCall
	accept bool
	err    error
	wait   chan struct{}
	left   error
}

func (o *ownerStub) DecideForOwner(ctx context.Context, prompt *approval.Prompt, requester *approval.Requester) (bool, error) {
	o.mu.Lock()
	o.calls = append(o.calls, ownerCall{prompt: *prompt, requester: *requester})
	wait := o.wait
	o.mu.Unlock()
	if wait != nil {
		select {
		case <-wait:
		case <-ctx.Done():
			o.mu.Lock()
			o.left = ctx.Err()
			o.mu.Unlock()
			return false, ctx.Err()
		}
	}
	return o.accept, o.err
}

func (o *ownerStub) recorded() []ownerCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]ownerCall(nil), o.calls...)
}

func (o *ownerStub) leftWith() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.left
}

type approvalProbe struct {
	approvals *Approvals
	principal string
	mu        sync.Mutex
	turn      runtime.TurnRequest
	held      bool
	accepted  bool
	err       error
}

func (p *approvalProbe) Run(ctx context.Context, req *runtime.TurnRequest) iter.Seq2[store.RuntimeEvent, error] {
	return func(yield func(store.RuntimeEvent, error) bool) {
		if !emit(yield, turnEvent(runtime.EventKindTurnAccepted)) {
			return
		}
		principalID := req.PrincipalID
		if p.principal != "" {
			principalID = p.principal
		}
		held := p.approvals.HoldsApprovalTarget(req.SessionID)
		accepted, err := p.approvals.Decide(ctx, &approval.Prompt{ToolName: "write_file", SessionID: req.SessionID, PrincipalID: principalID})
		p.mu.Lock()
		p.turn, p.held, p.accepted, p.err = *req, held, accepted, err
		p.mu.Unlock()
		text := "the tool ran"
		if !accepted {
			text = "the tool was denied"
		}
		emit(yield, answerEvent(text), turnEvent(runtime.EventKindTurnCompleted))
	}
}

func (p *approvalProbe) observed() (turn runtime.TurnRequest, held, accepted bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.turn, p.held, p.accepted, p.err
}

func newApprovalFixture(t *testing.T, owner *ownerStub, probeOverride string) (*chatFixture, *approvalProbe, *Approvals) {
	t.Helper()
	approvals, err := NewApprovals(owner)
	if err != nil {
		t.Fatalf("NewApprovals: %v", err)
	}
	probe := &approvalProbe{approvals: approvals, principal: probeOverride}
	return newChatFixture(t, chatFixtureOptions{backend: probe, approvals: approvals}), probe, approvals
}

func TestApprovalsRouteAnHTTPTurnToTheOwnerApprover(t *testing.T) {
	cases := []struct {
		name       string
		class      principal.Class
		stream     bool
		accept     bool
		wantAnswer string
	}{
		{"public completion accepted", principal.ClassPublic, false, true, "the tool ran"},
		{"trusted completion rejected", principal.ClassTrusted, false, false, "the tool was denied"},
		{"owner completion accepted", principal.ClassOwner, false, true, "the tool ran"},
		{"public stream accepted", principal.ClassPublic, true, true, "the tool ran"},
		{"trusted stream rejected", principal.ClassTrusted, true, false, "the tool was denied"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := &ownerStub{accept: tc.accept}
			fixture, probe, approvals := newApprovalFixture(t, owner, "")
			issued := fixture.keys.add(t, tc.class, testPersona)
			body := chatBody(userMessage("write it"))
			if tc.stream {
				body = defaultStreamBody()
			}
			rec := httptest.NewRecorder()
			fixture.handler.ServeHTTP(rec, jsonPost(t, issued.Plaintext, body))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			if tc.stream {
				if got := contentOf(t, sseFrames(t, rec.Body.String())); got != tc.wantAnswer {
					t.Errorf("streamed %q, want %q", got, tc.wantAnswer)
				}
			} else if !strings.Contains(rec.Body.String(), tc.wantAnswer) {
				t.Errorf("body %s lacks %q", rec.Body.String(), tc.wantAnswer)
			}
			turn, held, accepted, err := probe.observed()
			if err != nil || accepted != tc.accept || !held {
				t.Errorf("Decide() = %v, %v with the session held = %v; want accepted=%v while the turn runs", accepted, err, held, tc.accept)
			}
			calls := owner.recorded()
			want := approval.Requester{Principal: turn.PrincipalID, Class: tc.class, Persona: testPersona}
			if len(calls) != 1 || calls[0].requester != want || calls[0].prompt.SessionID != turn.SessionID || calls[0].prompt.PrincipalID != turn.PrincipalID {
				t.Errorf("owner calls = %+v, want one for requester %+v", calls, want)
			}
			if approvals.HoldsApprovalTarget(turn.SessionID) {
				t.Error("the session is still held after the turn ended")
			}
		})
	}
}

func TestApprovalsDenyWhenThePromptPrincipalIsNotTheRequester(t *testing.T) {
	owner := &ownerStub{accept: true}
	fixture, probe, _ := newApprovalFixture(t, owner, "http:someone-else")
	issued := fixture.keys.add(t, principal.ClassPublic, testPersona)
	rec := httptest.NewRecorder()
	fixture.handler.ServeHTTP(rec, jsonPost(t, issued.Plaintext, chatBody(userMessage("write it"))))
	_, _, accepted, err := probe.observed()
	if accepted || err != nil {
		t.Errorf("Decide() = %v, %v; want a plain denial", accepted, err)
	}
	if calls := owner.recorded(); len(calls) != 0 {
		t.Errorf("owner was asked for a foreign principal: %+v", calls)
	}
	if !strings.Contains(rec.Body.String(), "the tool was denied") {
		t.Errorf("body %s lacks the denial", rec.Body.String())
	}
}

func TestApprovalsDenyUnregisteredSessions(t *testing.T) {
	owner := &ownerStub{accept: true}
	approvals, err := NewApprovals(owner)
	if err != nil {
		t.Fatalf("NewApprovals: %v", err)
	}
	if approvals.HoldsApprovalTarget("lobby/http/key/ended") {
		t.Error("an unknown session is held")
	}
	accepted, err := approvals.Decide(t.Context(), &approval.Prompt{ToolName: "write_file", SessionID: "lobby/http/key/ended", PrincipalID: "http:key"})
	if accepted || err != nil {
		t.Errorf("Decide() = %v, %v; want a plain denial", accepted, err)
	}
	if calls := owner.recorded(); len(calls) != 0 {
		t.Errorf("owner was asked for an unregistered session: %+v", calls)
	}
}

func TestApprovalsPropagateTheOwnerApproverFailure(t *testing.T) {
	failure := errors.New("owner did not answer")
	owner := &ownerStub{err: failure}
	fixture, probe, _ := newApprovalFixture(t, owner, "")
	issued := fixture.keys.add(t, principal.ClassPublic, testPersona)
	rec := httptest.NewRecorder()
	fixture.handler.ServeHTTP(rec, jsonPost(t, issued.Plaintext, chatBody(userMessage("write it"))))
	_, _, accepted, err := probe.observed()
	if accepted || !errors.Is(err, failure) {
		t.Errorf("Decide() = %v, %v; want a denial carrying the approver error", accepted, err)
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "the tool was denied") {
		t.Errorf("reply = %d %s, want the turn to finish with the tool denied", rec.Code, rec.Body.String())
	}
}

func TestApprovalsRejectInvalidArguments(t *testing.T) {
	approvals, err := NewApprovals(&ownerStub{})
	if err != nil {
		t.Fatalf("NewApprovals: %v", err)
	}
	var missing context.Context
	valid := approval.Prompt{ToolName: "t", SessionID: "s", PrincipalID: "p"}
	blank := approval.Prompt{ToolName: " ", SessionID: "s", PrincipalID: "p"}
	cases := []struct {
		name   string
		decide func() (bool, error)
	}{
		{"nil context", func() (bool, error) { return approvals.Decide(missing, &valid) }},
		{"nil prompt", func() (bool, error) { return approvals.Decide(t.Context(), nil) }},
		{"blank tool", func() (bool, error) { return approvals.Decide(t.Context(), &blank) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accepted, err := tc.decide()
			var coded *Error
			if accepted || !errors.As(err, &coded) || coded.Code != ErrorCodeInvalidArgument {
				t.Errorf("Decide() = %v, %v; want a denial with invalid_argument", accepted, err)
			}
		})
	}
	if _, err := NewApprovals(nil); err == nil {
		t.Error("NewApprovals accepted a nil owner approver")
	}
}

func TestApprovalsHoldASessionUntilEveryTurnReleasesIt(t *testing.T) {
	approvals, err := NewApprovals(&ownerStub{})
	if err != nil {
		t.Fatalf("NewApprovals: %v", err)
	}
	requester := &approval.Requester{Principal: "http:key", Class: principal.ClassPublic, Persona: testPersona}
	first := approvals.hold("s", requester)
	second := approvals.hold("s", requester)
	if got, ok := approvals.RequesterOf("s"); !ok || got != *requester {
		t.Fatalf("RequesterOf during the turns = %+v, %v; want %+v", got, ok, *requester)
	}
	first()
	first()
	if !approvals.HoldsApprovalTarget("s") {
		t.Fatal("releasing one turn twice dropped the other turn's session")
	}
	second()
	if approvals.HoldsApprovalTarget("s") {
		t.Fatal("the session stayed held after every turn released it")
	}
	if got, ok := approvals.RequesterOf("s"); ok {
		t.Fatalf("RequesterOf after release = %+v, want none", got)
	}
	var absent *Approvals
	absent.hold("s", requester)()
}

func TestStreamKeepaliveFlowsWhileTheOwnerDecides(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		owner := &ownerStub{accept: true, wait: make(chan struct{})}
		fixture, probe, approvals := newApprovalFixture(t, owner, "")
		issued := fixture.keys.add(t, principal.ClassPublic, testPersona)
		w := newCaptureWriter()
		served := fixture.serveAsync(jsonPost(t, issued.Plaintext, defaultStreamBody()).WithContext(t.Context()), w)
		synctest.Wait()
		if got := len(owner.recorded()); got != 1 {
			t.Fatalf("owner calls = %d, want the turn blocked on one owner decision", got)
		}

		time.Sleep(2*testKeepalive + time.Second)
		synctest.Wait()
		if _, body := w.snapshot(); body != strings.Repeat(frameKeepalives+"\n\n", 2) {
			t.Fatalf("body while the owner decides = %q, want two keepalives", body)
		}

		close(owner.wait)
		synctest.Wait()
		select {
		case <-served:
		default:
			t.Fatal("handler is still running after the owner answered")
		}
		_, body := w.snapshot()
		frames := sseFrames(t, body)
		if got := contentOf(t, frames); got != "the tool ran" || frames[len(frames)-1] != frameDoneMarker {
			t.Errorf("streamed %q ending %q, want the accepted answer and DONE", got, frames[len(frames)-1])
		}
		turn, _, _, _ := probe.observed()
		if approvals.HoldsApprovalTarget(turn.SessionID) {
			t.Error("the session is still held after the turn ended")
		}
	})
}

func TestClientDisconnectEndsTheOwnerWait(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"completion", chatBody(userMessage("write it"))},
		{"stream", defaultStreamBody()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				owner := &ownerStub{accept: true, wait: make(chan struct{})}
				fixture, probe, approvals := newApprovalFixture(t, owner, "")
				issued := fixture.keys.add(t, principal.ClassPublic, testPersona)
				ctx, cancel := context.WithCancel(t.Context())
				served := fixture.serveAsync(jsonPost(t, issued.Plaintext, tc.body).WithContext(ctx), newCaptureWriter())
				synctest.Wait()
				if got := len(owner.recorded()); got != 1 {
					t.Fatalf("owner calls = %d, want the turn blocked on one owner decision", got)
				}

				cancel()
				synctest.Wait()

				select {
				case <-served:
				default:
					t.Fatal("handler is still running after the client left")
				}
				if err := owner.leftWith(); !errors.Is(err, context.Canceled) {
					t.Errorf("owner wait ended with %v, want cancellation", err)
				}
				turn, _, accepted, err := probe.observed()
				if accepted || !errors.Is(err, context.Canceled) {
					t.Errorf("Decide() = %v, %v; want a denial carrying the cancellation", accepted, err)
				}
				if approvals.HoldsApprovalTarget(turn.SessionID) {
					t.Error("the session is still held after the client left")
				}
			})
		})
	}
}

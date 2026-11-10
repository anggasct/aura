package approval

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
)

type routeOwner struct {
	mu     sync.Mutex
	calls  []Requester
	accept bool
	err    error
}

func (o *routeOwner) DecideForOwner(_ context.Context, _ *Prompt, requester *Requester) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, *requester)
	return o.accept, o.err
}

func (o *routeOwner) recorded() []Requester {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Requester(nil), o.calls...)
}

func newTestRoute(t *testing.T, owner OwnerApprover) *OwnerRoute {
	t.Helper()
	route, err := NewOwnerRoute(owner)
	if err != nil {
		t.Fatalf("NewOwnerRoute: %v", err)
	}
	return route
}

func TestOwnerRouteDecidesForTheHeldRequester(t *testing.T) {
	requester := &Requester{Principal: "email:alice@example.com", Class: principal.ClassPublic, Persona: "support"}
	cases := []struct {
		name       string
		hold       func(route *OwnerRoute)
		principal  string
		accept     bool
		ownerErr   error
		wantAsked  int
		wantAccept bool
		wantErr    error
	}{
		{name: "held and accepted", hold: func(r *OwnerRoute) { r.Hold("s", requester) }, principal: requester.Principal, accept: true, wantAsked: 1, wantAccept: true},
		{name: "held and rejected", hold: func(r *OwnerRoute) { r.Hold("s", requester) }, principal: requester.Principal, wantAsked: 1},
		{name: "remembered and accepted", hold: func(r *OwnerRoute) { r.Remember("s", requester) }, principal: requester.Principal, accept: true, wantAsked: 1, wantAccept: true},
		{name: "owner failure propagates", hold: func(r *OwnerRoute) { r.Hold("s", requester) }, principal: requester.Principal, ownerErr: errors.New("unreachable"), wantAsked: 1, wantErr: errors.New("unreachable")},
		{name: "foreign principal", hold: func(r *OwnerRoute) { r.Hold("s", requester) }, principal: "email:mallory@example.com", accept: true},
		{name: "unknown session", hold: func(*OwnerRoute) {}, principal: requester.Principal, accept: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := &routeOwner{accept: tc.accept, err: tc.ownerErr}
			route := newTestRoute(t, owner)
			tc.hold(route)
			accepted, err := route.Decide(t.Context(), &Prompt{ToolName: "write_file", SessionID: "s", PrincipalID: tc.principal})
			if accepted != tc.wantAccept {
				t.Errorf("accepted = %v, want %v", accepted, tc.wantAccept)
			}
			if (err == nil) != (tc.wantErr == nil) || (err != nil && err.Error() != tc.wantErr.Error()) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if got := owner.recorded(); len(got) != tc.wantAsked || (tc.wantAsked > 0 && got[0] != *requester) {
				t.Errorf("owner asked %+v, want %d call(s) for %+v", got, tc.wantAsked, *requester)
			}
		})
	}
}

func TestOwnerRouteRejectsInvalidArguments(t *testing.T) {
	route := newTestRoute(t, &routeOwner{})
	var missing context.Context
	valid := Prompt{ToolName: "t", SessionID: "s", PrincipalID: "p"}
	cases := []struct {
		name   string
		decide func() (bool, error)
	}{
		{"nil context", func() (bool, error) { return route.Decide(missing, &valid) }},
		{"nil prompt", func() (bool, error) { return route.Decide(t.Context(), nil) }},
		{"blank tool", func() (bool, error) {
			return route.Decide(t.Context(), &Prompt{ToolName: " ", SessionID: "s", PrincipalID: "p"})
		}},
		{"blank session", func() (bool, error) {
			return route.Decide(t.Context(), &Prompt{ToolName: "t", SessionID: "", PrincipalID: "p"})
		}},
		{"blank principal", func() (bool, error) { return route.Decide(t.Context(), &Prompt{ToolName: "t", SessionID: "s"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accepted, err := tc.decide()
			if code, ok := CodeOf(err); accepted || !ok || code != ErrorCodeInvalidArgument {
				t.Errorf("Decide() = %v, %v; want a denial with invalid_argument", accepted, err)
			}
		})
	}
	if _, err := NewOwnerRoute(nil); err == nil {
		t.Error("NewOwnerRoute accepted a nil owner approver")
	}
}

func TestOwnerRouteHoldsASessionUntilEveryTurnReleasesIt(t *testing.T) {
	route := newTestRoute(t, &routeOwner{})
	requester := &Requester{Principal: "http:key", Class: principal.ClassPublic, Persona: "support"}
	first := route.Hold("s", requester)
	second := route.Hold("s", requester)
	if got, ok := route.RequesterOf("s"); !ok || got != *requester {
		t.Fatalf("RequesterOf during the turns = %+v, %v; want %+v", got, ok, *requester)
	}
	first()
	first()
	if !route.HoldsApprovalTarget("s") {
		t.Fatal("releasing one turn twice dropped the other turn's session")
	}
	second()
	if route.HoldsApprovalTarget("s") {
		t.Fatal("the session stayed held after every turn released it")
	}
	if got, ok := route.RequesterOf("s"); ok {
		t.Fatalf("RequesterOf after release = %+v, want none", got)
	}
	var absent *OwnerRoute
	absent.Hold("s", requester)()
	absent.Remember("s", requester)
	route.Hold("s", nil)()
	route.Remember("s", nil)
}

func TestOwnerRouteKeepsRememberedSessionsBounded(t *testing.T) {
	route := newTestRoute(t, &routeOwner{})
	requester := &Requester{Principal: "email:alice@example.com", Class: principal.ClassPublic, Persona: "support"}
	release := route.Hold("held", requester)
	route.Remember("held", requester)
	release()
	if !route.HoldsApprovalTarget("held") {
		t.Fatal("a remembered session was dropped when its turn released it")
	}
	for i := range maxRemembered + 10 {
		route.Remember("session-"+strconv.Itoa(i), requester)
	}
	route.mu.Lock()
	size, tracked := len(route.held), route.remembered
	route.mu.Unlock()
	if size > maxRemembered+1 || tracked > maxRemembered+1 {
		t.Errorf("registry holds %d sessions (%d remembered), want at most %d", size, tracked, maxRemembered+1)
	}
	live := route.Hold("live", requester)
	for i := range maxRemembered + 10 {
		route.Remember("later-"+strconv.Itoa(i), requester)
	}
	if !route.HoldsApprovalTarget("live") {
		t.Error("eviction dropped a session with a turn in flight")
	}
	live()
}

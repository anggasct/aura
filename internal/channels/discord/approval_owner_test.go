package discord

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/config"
	policy "github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
)

const httpPrincipal = "http:key-1"

func httpRequester(class principal.Class) *approval.Requester {
	return &approval.Requester{Principal: httpPrincipal, Class: class, Persona: "team"}
}

func httpPrompt(expiresAt time.Time) *ApprovalPrompt {
	prompt := testPrompt("http-session", expiresAt)
	prompt.PrincipalID = httpPrincipal
	prompt.ToolName = "write_file"
	return prompt
}

func newHTTPOwnerAdapter(t *testing.T, stub *approvalStub, owners []string, personas []config.AgentDefinition) (*Adapter, *accessObserver) {
	t.Helper()
	adapter, _, observer := newAccessAdapter(t, "team", owners, personas)
	adapter.restBase = stub.srv.URL
	return adapter, observer
}

func decideForOwnerAsync(ctx context.Context, adapter *Adapter, prompt *ApprovalPrompt, requester *approval.Requester) <-chan decideResult {
	out := make(chan decideResult, 1)
	go func() {
		accepted, err := adapter.DecideForOwner(ctx, prompt, requester)
		out <- decideResult{accepted: accepted, err: err}
	}()
	return out
}

func TestDecideForOwnerOpensTheOwnerDMCardWithoutRequesterNotices(t *testing.T) {
	cases := []struct {
		name       string
		button     int
		accept     bool
		wantRouted string
	}{
		{"owner approves", 0, true, ApprovalRoutedApproved},
		{"owner rejects", 2, false, ApprovalRoutedRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newApprovalStub(t)
			adapter, observer := newHTTPOwnerAdapter(t, stub, []string{"discord:777"}, accessPersonas())
			result := decideForOwnerAsync(t.Context(), adapter, httpPrompt(time.Now().Add(time.Minute)), httpRequester(principal.ClassTrusted))
			card := stub.waitPostTo(t, "555", 1)[0]
			content, _ := card.body["content"].(string)
			if !strings.Contains(content, "persona: Team (team)") || !strings.Contains(content, "write_file") {
				t.Fatalf("owner card content = %q", content)
			}
			labels, ids := cardButtons(t, card)
			if strings.Join(labels, "|") != "Approve|Allow 7 days|Reject" {
				t.Fatalf("owner card buttons = %v", labels)
			}
			adapter.resolveComponentClick(t.Context(), clickPayload("ix-requester", "111", "", "555", ids[0]))
			select {
			case outcome := <-result:
				t.Fatalf("requester click decided the approval: %+v", outcome)
			default:
			}
			adapter.resolveComponentClick(t.Context(), clickPayload("ix-owner", "777", "", "555", ids[tc.button]))
			if outcome := awaitDecide(t, result, 5*time.Second); outcome.err != nil || outcome.accepted != tc.accept {
				t.Fatalf("DecideForOwner() = %v, %v; want accepted=%v", outcome.accepted, outcome.err, tc.accept)
			}
			if posts, _, patches := stub.counts(); posts != 1 || patches != 1 {
				t.Fatalf("posts = %d, card disables = %d; want the card alone and one disable", posts, patches)
			}
			if observer.count(observer.routed, tc.wantRouted) != 1 {
				t.Fatalf("routed = %v, want one %s", observer.routed, tc.wantRouted)
			}
		})
	}
}

func TestDecideForOwnerRecordsAStandingGrantForTrustedRequesters(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, observer := newHTTPOwnerAdapter(t, stub, []string{"discord:777"}, accessPersonas())
	result := decideForOwnerAsync(t.Context(), adapter, httpPrompt(time.Now().Add(time.Minute)), httpRequester(principal.ClassTrusted))
	_, ids := cardButtons(t, stub.waitPostTo(t, "555", 1)[0])
	adapter.resolveComponentClick(t.Context(), clickPayload("ix-standing", "777", "", "555", ids[1]))
	if outcome := awaitDecide(t, result, 5*time.Second); outcome.err != nil || !outcome.accepted {
		t.Fatalf("standing click = %v, %v; want accepted", outcome.accepted, outcome.err)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	want := StandingGrantRequest{PrincipalID: httpPrincipal, AgentID: "team", ToolName: "write_file", OwnerID: "discord:777", TTL: 168 * time.Hour}
	if len(observer.grants) != 1 || observer.grants[0] != want {
		t.Fatalf("standing grants = %+v, want %+v", observer.grants, want)
	}
}

func TestDecideForOwnerOffersNoStandingGrantToPublicOrOwnerRequesters(t *testing.T) {
	cases := []struct {
		name      string
		principal string
		class     principal.Class
	}{
		{"public requester", httpPrincipal, principal.ClassPublic},
		{"owner requester", principal.OwnerID, principal.ClassOwner},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newApprovalStub(t)
			adapter, _ := newHTTPOwnerAdapter(t, stub, []string{"discord:777"}, accessPersonas())
			prompt := httpPrompt(time.Now().Add(time.Minute))
			prompt.PrincipalID = tc.principal
			requester := &approval.Requester{Principal: tc.principal, Class: tc.class, Persona: "team"}
			result := decideForOwnerAsync(t.Context(), adapter, prompt, requester)
			labels, ids := cardButtons(t, stub.waitPostTo(t, "555", 1)[0])
			if strings.Join(labels, "|") != "Approve|Reject" {
				t.Fatalf("card buttons = %v, want no standing grant", labels)
			}
			adapter.resolveComponentClick(t.Context(), clickPayload("ix-reject", "777", "", "555", ids[1]))
			if outcome := awaitDecide(t, result, 5*time.Second); outcome.err != nil || outcome.accepted {
				t.Fatalf("DecideForOwner() = %v, %v; want the rejection applied", outcome.accepted, outcome.err)
			}
		})
	}
}

func TestDecideForOwnerOffersNoStandingGrantForUntrustedInput(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"approval_required", "Approve|Allow 7 days|Reject"},
		{policy.ReasonUntrustedInput, "Approve|Reject"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			stub := newApprovalStub(t)
			adapter, _ := newHTTPOwnerAdapter(t, stub, []string{"discord:777"}, accessPersonas())
			prompt := httpPrompt(time.Now().Add(time.Minute))
			prompt.ReasonCode = tc.reason
			result := decideForOwnerAsync(t.Context(), adapter, prompt, httpRequester(principal.ClassTrusted))
			labels, ids := cardButtons(t, stub.waitPostTo(t, "555", 1)[0])
			if strings.Join(labels, "|") != tc.want {
				t.Fatalf("card buttons = %v, want %s", labels, tc.want)
			}
			adapter.resolveComponentClick(t.Context(), clickPayload("ix-reject", "777", "", "555", ids[len(ids)-1]))
			if outcome := awaitDecide(t, result, 5*time.Second); outcome.err != nil || outcome.accepted {
				t.Fatalf("DecideForOwner() = %v, %v; want the rejection applied", outcome.accepted, outcome.err)
			}
		})
	}
}

func TestDecideForOwnerTimesOutWithTheOwnerApprovalTimeout(t *testing.T) {
	stub := newApprovalStub(t)
	personas := accessPersonas()
	personas[1].ApprovalTimeout = config.Duration(80 * time.Millisecond)
	adapter, observer := newHTTPOwnerAdapter(t, stub, []string{"discord:777"}, personas)
	accepted, err := adapter.DecideForOwner(t.Context(), httpPrompt(time.Now().Add(time.Hour)), httpRequester(principal.ClassPublic))
	if code, ok := CodeOf(err); accepted || !ok || code != ErrorCodeApprovalTimeout {
		t.Fatalf("DecideForOwner() = %v, %v; want approval_timeout", accepted, err)
	}
	if posts, _, patches := stub.counts(); posts != 1 || patches != 1 {
		t.Fatalf("posts = %d, card disables = %d; want the card alone and one disable", posts, patches)
	}
	if observer.count(observer.routed, ApprovalRoutedTimeout) != 1 {
		t.Fatalf("routed = %v, want one timeout", observer.routed)
	}
}

func TestDecideForOwnerUnavailableDenies(t *testing.T) {
	cases := map[string]struct {
		owners []string
		dmCode int
	}{
		"no owner discord identity": {owners: nil, dmCode: http.StatusOK},
		"owner dm cannot be opened": {owners: []string{"discord:777"}, dmCode: http.StatusForbidden},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := newApprovalStub(t)
			stub.dmCode = tc.dmCode
			adapter, observer := newHTTPOwnerAdapter(t, stub, tc.owners, accessPersonas())
			accepted, err := adapter.DecideForOwner(t.Context(), httpPrompt(time.Now().Add(time.Minute)), httpRequester(principal.ClassTrusted))
			if code, ok := CodeOf(err); accepted || !ok || code != ErrorCodeApprovalUnavailable {
				t.Fatalf("DecideForOwner() = %v, %v; want approval_unavailable", accepted, err)
			}
			if posts, _, _ := stub.counts(); posts != 0 {
				t.Fatalf("posts = %d, want no card", posts)
			}
			if observer.count(observer.routed, ApprovalRoutedUnavailable) != 1 {
				t.Fatalf("routed = %v, want one unavailable", observer.routed)
			}
		})
	}
}

func TestDecideForOwnerRejectsInvalidArguments(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, _ := newHTTPOwnerAdapter(t, stub, []string{"discord:777"}, accessPersonas())
	var missing context.Context
	expires := time.Now().Add(time.Minute)
	cases := []struct {
		name   string
		decide func() (bool, error)
	}{
		{"nil context", func() (bool, error) {
			return adapter.DecideForOwner(missing, httpPrompt(expires), httpRequester(principal.ClassPublic))
		}},
		{"nil prompt", func() (bool, error) {
			return adapter.DecideForOwner(t.Context(), nil, httpRequester(principal.ClassPublic))
		}},
		{"nil requester", func() (bool, error) { return adapter.DecideForOwner(t.Context(), httpPrompt(expires), nil) }},
		{"requester for another principal", func() (bool, error) {
			return adapter.DecideForOwner(t.Context(), httpPrompt(expires), &approval.Requester{Principal: "http:key-2", Persona: "team"})
		}},
		{"blank requester principal", func() (bool, error) {
			return adapter.DecideForOwner(t.Context(), httpPrompt(expires), &approval.Requester{Persona: "team"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accepted, err := tc.decide()
			if code, ok := CodeOf(err); accepted || !ok || code != ErrorCodeInvalidArgument {
				t.Errorf("DecideForOwner() = %v, %v; want a denial with invalid_argument", accepted, err)
			}
		})
	}
	if posts, _, _ := stub.counts(); posts != 0 {
		t.Errorf("posts = %d, want none for invalid requests", posts)
	}
}

func TestServesPersonaFollowsTheAccountBinding(t *testing.T) {
	stub := newApprovalStub(t)
	adapter, _ := newHTTPOwnerAdapter(t, stub, []string{"discord:777"}, accessPersonas())
	cases := []struct {
		persona string
		want    bool
	}{
		{"team", true},
		{"lobby", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run("persona "+tc.persona, func(t *testing.T) {
			if got := adapter.ServesPersona(tc.persona); got != tc.want {
				t.Errorf("ServesPersona(%q) = %v, want %v", tc.persona, got, tc.want)
			}
		})
	}
	adapter.SetPersonaRouter(nil)
	if adapter.ServesPersona("team") {
		t.Error("an adapter without a router serves a persona")
	}
}

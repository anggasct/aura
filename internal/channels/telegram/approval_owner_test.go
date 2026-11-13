package telegram

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	policy "github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
)

const httpPrincipal = "http:key-1"

func httpRequester(class principal.Class) *approval.Requester {
	return &approval.Requester{Principal: httpPrincipal, Class: class, Persona: testPersona}
}

func (r *approvalRig) decideForOwner(ctx context.Context, prompt *approval.Prompt, requester *approval.Requester) <-chan decision {
	out := make(chan decision, 1)
	go func() {
		accepted, err := r.adapter.DecideForOwner(ctx, prompt, requester)
		out <- decision{accepted: accepted, err: err}
	}()
	return out
}

func (r *approvalRig) plainMessages() []string {
	var texts []string
	for _, call := range r.h.api.callsOf("sendMessage") {
		if call.body["reply_markup"] != nil {
			continue
		}
		text, _ := call.body["text"].(string)
		texts = append(texts, text)
	}
	return texts
}

func TestDecideForOwnerOpensTheOwnerCardWithoutRequesterNotices(t *testing.T) {
	cases := []struct {
		name       string
		label      string
		accept     bool
		wantRouted []string
	}{
		{"owner approves", "Approve", true, []string{approval.RoutedApproved}},
		{"owner rejects", "Reject", false, []string{approval.RoutedRejected}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApprovalRig(t, accessOpts{definition: routedDefinition()})
			synctest.Test(t, func(t *testing.T) {
				result := rig.decideForOwner(t.Context(), approvalPrompt("http-session", httpPrincipal), httpRequester(principal.ClassPublic))
				card := rig.mustCard(t, 1)
				if card.chatID != ownerUserID || card.threadID != 0 {
					t.Fatalf("card posted to %d/%d, want the owner's private chat", card.chatID, card.threadID)
				}
				rig.press(t, "cb-requester", requesterID, card, card.data(t, "Approve"))
				if _, done := settled(result); done {
					t.Fatal("the requester decided its own approval")
				}
				rig.press(t, "cb-owner", ownerUserID, card, card.data(t, tc.label))
				outcome, done := settled(result)
				if !done || outcome.err != nil || outcome.accepted != tc.accept {
					t.Fatalf("decision = %+v (settled %v), want accepted=%v", outcome, done, tc.accept)
				}
				if got := rig.plainMessages(); len(got) != 0 {
					t.Errorf("plain messages = %q, want no requester notices", got)
				}
				if got := rig.routed.recorded(); !slices.Equal(got, tc.wantRouted) {
					t.Errorf("routed observations = %v, want %v", got, tc.wantRouted)
				}
				assertKeyboardRemoved(t, rig, card)
			})
		})
	}
}

func TestDecideForOwnerTimesOutWithTheOwnerApprovalTimeout(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{definition: routedDefinition()})
	synctest.Test(t, func(t *testing.T) {
		prompt := approvalPrompt("http-session", httpPrincipal)
		prompt.ExpiresAt = time.Now().Add(time.Hour)
		result := rig.decideForOwner(t.Context(), prompt, httpRequester(principal.ClassPublic))
		card := rig.mustCard(t, 1)
		time.Sleep(29 * time.Second)
		synctest.Wait()
		if _, done := settled(result); done {
			t.Fatal("decision settled before the owner timeout")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		outcome, done := settled(result)
		code, coded := CodeOf(outcome.err)
		if !done || outcome.accepted || !coded || code != ErrorCodeApprovalTimeout {
			t.Fatalf("decision = %+v (settled %v), want a denial with approval_timeout", outcome, done)
		}
		if got := rig.routed.recorded(); !slices.Equal(got, []string{approval.RoutedTimeout}) {
			t.Errorf("routed observations = %v", got)
		}
		if got := rig.plainMessages(); len(got) != 0 {
			t.Errorf("plain messages = %q, want no requester notices", got)
		}
		assertKeyboardRemoved(t, rig, card)
	})
}

func TestDecideForOwnerOffersStandingGrantsByClass(t *testing.T) {
	cases := []struct {
		name       string
		class      principal.Class
		wantLabels []string
		wantGrants int
	}{
		{"trusted requester", principal.ClassTrusted, []string{"Approve", "Allow 7 days", "Reject"}, 1},
		{"public requester", principal.ClassPublic, []string{"Approve", "Reject"}, 0},
		{"owner requester", principal.ClassOwner, []string{"Approve", "Reject"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApprovalRig(t, accessOpts{definition: routedDefinition()})
			synctest.Test(t, func(t *testing.T) {
				result := rig.decideForOwner(t.Context(), approvalPrompt("http-session", httpPrincipal), httpRequester(tc.class))
				card := rig.mustCard(t, 1)
				var labels []string
				for _, button := range card.buttons {
					labels = append(labels, button.text)
				}
				if !slices.Equal(labels, tc.wantLabels) {
					t.Fatalf("card buttons = %v, want %v", labels, tc.wantLabels)
				}
				answer := "Approve"
				if tc.wantGrants == 1 {
					answer = "Allow 7 days"
				}
				rig.press(t, "cb-owner", ownerUserID, card, card.data(t, answer))
				if outcome, done := settled(result); !done || outcome.err != nil || !outcome.accepted {
					t.Fatalf("decision = %+v (settled %v), want accepted", outcome, done)
				}
				grants := rig.grants.recorded()
				if len(grants) != tc.wantGrants {
					t.Fatalf("recorded grants = %+v, want %d", grants, tc.wantGrants)
				}
				if tc.wantGrants == 1 {
					want := approval.StandingGrantRequest{
						PrincipalID: httpPrincipal,
						AgentID:     testPersona,
						ToolName:    "shell.exec",
						OwnerID:     "telegram:" + strconv.Itoa(ownerUserID),
						TTL:         7 * 24 * time.Hour,
					}
					if grants[0] != want {
						t.Errorf("recorded grant = %+v, want %+v", grants[0], want)
					}
				}
			})
		})
	}
}

func TestOwnerCardOffersNoStandingGrantForUntrustedInput(t *testing.T) {
	cases := []struct {
		reason     string
		wantLabels []string
	}{
		{"approval_required", []string{"Approve", "Allow 7 days", "Reject"}},
		{policy.ReasonUntrustedInput, []string{"Approve", "Reject"}},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			rig := newApprovalRig(t, accessOpts{definition: routedDefinition()})
			synctest.Test(t, func(t *testing.T) {
				prompt := approvalPrompt("http-session", httpPrincipal)
				prompt.ReasonCode = tc.reason
				result := rig.decideForOwner(t.Context(), prompt, httpRequester(principal.ClassTrusted))
				card := rig.mustCard(t, 1)
				var labels []string
				for _, button := range card.buttons {
					labels = append(labels, button.text)
				}
				if !slices.Equal(labels, tc.wantLabels) {
					t.Fatalf("card buttons = %v, want %v", labels, tc.wantLabels)
				}
				rig.press(t, "cb-owner", ownerUserID, card, card.data(t, "Approve"))
				if outcome, done := settled(result); !done || outcome.err != nil || !outcome.accepted {
					t.Fatalf("decision = %+v (settled %v), want accepted", outcome, done)
				}
				if grants := rig.grants.recorded(); len(grants) != 0 {
					t.Fatalf("recorded grants = %+v, want none", grants)
				}
			})
		})
	}
}

func TestDecideForOwnerWithoutATelegramOwnerIsUnavailable(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{definition: routedDefinition()})
	owners, err := principal.NewResolver([]string{"discord:123456789012345678"})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	rig.adapter.SetPrincipalResolver(owners)
	synctest.Test(t, func(t *testing.T) {
		accepted, err := rig.adapter.DecideForOwner(t.Context(), approvalPrompt("http-session", httpPrincipal), httpRequester(principal.ClassPublic))
		if code, ok := CodeOf(err); accepted || !ok || code != ErrorCodeApprovalUnavailable {
			t.Fatalf("DecideForOwner() = %v, %v; want a denial with approval_unavailable", accepted, err)
		}
		if got := rig.routed.recorded(); !slices.Equal(got, []string{approval.RoutedUnavailable}) {
			t.Errorf("routed observations = %v", got)
		}
		if len(rig.cards()) != 0 {
			t.Error("a card was posted without an owner")
		}
	})
}

func TestDecideForOwnerRejectsInvalidArguments(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{definition: routedDefinition()})
	var missing context.Context
	cases := []struct {
		name   string
		decide func() (bool, error)
	}{
		{"nil context", func() (bool, error) {
			return rig.adapter.DecideForOwner(missing, approvalPrompt("s", httpPrincipal), httpRequester(principal.ClassPublic))
		}},
		{"nil prompt", func() (bool, error) {
			return rig.adapter.DecideForOwner(t.Context(), nil, httpRequester(principal.ClassPublic))
		}},
		{"nil requester", func() (bool, error) {
			return rig.adapter.DecideForOwner(t.Context(), approvalPrompt("s", httpPrincipal), nil)
		}},
		{"requester for another principal", func() (bool, error) {
			return rig.adapter.DecideForOwner(t.Context(), approvalPrompt("s", "http:key-2"), httpRequester(principal.ClassPublic))
		}},
		{"blank requester principal", func() (bool, error) {
			return rig.adapter.DecideForOwner(t.Context(), approvalPrompt("s", httpPrincipal), &approval.Requester{Persona: testPersona})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accepted, err := tc.decide()
			code, ok := CodeOf(err)
			if accepted || !ok || code != ErrorCodeInvalidArgument {
				t.Errorf("DecideForOwner() = %v, %v; want a denial with invalid_argument", accepted, err)
			}
		})
	}
	if len(rig.cards()) != 0 {
		t.Error("a card was posted for an invalid request")
	}
}

func TestServesPersonaFollowsTheAccountBinding(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{definition: routedDefinition()})
	cases := []struct {
		persona string
		want    bool
	}{
		{testPersona, true},
		{"other", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run("persona "+tc.persona, func(t *testing.T) {
			if got := rig.adapter.ServesPersona(tc.persona); got != tc.want {
				t.Errorf("ServesPersona(%q) = %v, want %v", tc.persona, got, tc.want)
			}
		})
	}
}

func TestApprovalCardDisablesLinkPreviews(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{definition: routedDefinition()})
	synctest.Test(t, func(t *testing.T) {
		prompt := approvalPrompt("http-session", httpPrincipal)
		prompt.Arguments = "body: see https://evil.example/path"
		result := rig.decideForOwner(t.Context(), prompt, httpRequester(principal.ClassTrusted))
		card := rig.mustCard(t, 1)
		rig.press(t, "cb-owner", ownerUserID, card, card.data(t, "Approve"))
		if outcome, done := settled(result); !done || outcome.err != nil || !outcome.accepted {
			t.Fatalf("decision = %+v (settled %v), want accepted", outcome, done)
		}
		sent := rig.h.api.sends()
		options, _ := sent[0]["link_preview_options"].(map[string]any)
		if options["is_disabled"] != true {
			t.Errorf("link_preview_options = %v, want is_disabled true", sent[0]["link_preview_options"])
		}
	})
	h := newHarness(t)
	if _, err := h.adapter(t, nil).Deliver(t.Context(), deliveryRequest(t, h, "turn-reply", "see https://evil.example/path")); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	reply := h.api.sends()[0]
	if _, present := reply["link_preview_options"]; present {
		t.Errorf("reply body = %v, want no link_preview_options", reply)
	}
}

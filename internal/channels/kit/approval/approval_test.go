package approval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	policy "github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	registry, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry(): %v", err)
	}
	return registry
}

func ownerBinding() *Binding {
	return &Binding{Principal: principal.OwnerID, Class: principal.ClassOwner, Actor: "111", Scope: [2]string{"222", "333"}}
}

func TestParseCallbackData(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		valid bool
	}{
		{"empty", "", false},
		{"wrong prefix", "bogus:0123456789abcdef:a:0123456789abcdef", false},
		{"short token", "aura:abc:a:0123456789abcdef", false},
		{"bad action", "aura:0123456789abcdef:x:0123456789abcdef", false},
		{"non-hex mac", "aura:0123456789abcdef:a:zzzzzzzzzzzzzzzz", false},
		{"uppercase hex", "aura:0123456789ABCDEF:a:0123456789abcdef", false},
		{"extra part", "aura:0123456789abcdef:a:0123456789abcdef:x", false},
		{"approve", "aura:0123456789abcdef:a:0123456789abcdef", true},
		{"reject", "aura:0123456789abcdef:r:fedcba9876543210", true},
		{"standing", "aura:0123456789abcdef:s:fedcba9876543210", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, ok := ParseCallbackData(tc.raw)
			if ok != tc.valid {
				t.Errorf("ParseCallbackData(%q) valid = %v, want %v", tc.raw, ok, tc.valid)
			}
		})
	}
}

func TestCallbackDataFitsTelegramLimit(t *testing.T) {
	registry := newTestRegistry(t)
	binding := ownerBinding()
	binding.Standing = true
	tokenID, _, err := registry.Issue(binding, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Issue(): %v", err)
	}
	callbacks := registry.Callbacks(tokenID, binding, time.Now().Add(time.Minute))
	for name, data := range map[string]string{"approve": callbacks.Approve, "reject": callbacks.Reject, "standing": callbacks.Standing} {
		if len(data) != 40 {
			t.Errorf("%s callback data length = %d, want 40", name, len(data))
		}
		if _, _, _, ok := ParseCallbackData(data); !ok {
			t.Errorf("%s callback data %q does not parse", name, data)
		}
	}
}

func TestStandingCallbackIsAbsentWithoutTheOffer(t *testing.T) {
	registry := newTestRegistry(t)
	binding := ownerBinding()
	tokenID, _, err := registry.Issue(binding, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Issue(): %v", err)
	}
	if got := registry.Callbacks(tokenID, binding, time.Now().Add(time.Minute)).Standing; got != "" {
		t.Errorf("standing callback = %q, want none", got)
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name     string
		standing bool
		mutate   func(data string) string
		presser  Presser
		want     bool
	}{
		{name: "owner approves", presser: Presser{Actor: "111", Scope: [2]string{"222", "333"}}, want: true},
		{name: "wrong actor", presser: Presser{Actor: "999", Scope: [2]string{"222", "333"}}},
		{name: "wrong first scope", presser: Presser{Actor: "111", Scope: [2]string{"223", "333"}}},
		{name: "wrong second scope", presser: Presser{Actor: "111", Scope: [2]string{"222", "334"}}},
		{
			name:    "forged mac",
			mutate:  func(data string) string { return data[:len(data)-1] + flip(data[len(data)-1]) },
			presser: Presser{Actor: "111", Scope: [2]string{"222", "333"}},
		},
		{
			name:    "forged standing action",
			mutate:  func(data string) string { return strings.Replace(data, ":a:", ":s:", 1) },
			presser: Presser{Actor: "111", Scope: [2]string{"222", "333"}},
		},
		{
			name:    "unparseable data",
			mutate:  func(string) string { return "not-a-callback" },
			presser: Presser{Actor: "111", Scope: [2]string{"222", "333"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := newTestRegistry(t)
			binding := ownerBinding()
			binding.Standing = tc.standing
			expiresAt := time.Now().Add(time.Minute)
			tokenID, pending, err := registry.Issue(binding, expiresAt)
			if err != nil {
				t.Fatalf("Issue(): %v", err)
			}
			data := registry.Callbacks(tokenID, binding, expiresAt).Approve
			if tc.mutate != nil {
				data = tc.mutate(data)
			}
			if got := registry.Resolve(data, &tc.presser); got != tc.want {
				t.Fatalf("Resolve() = %v, want %v", got, tc.want)
			}
			select {
			case outcome := <-pending.result:
				if !tc.want || !outcome.Accepted || outcome.Standing {
					t.Errorf("outcome = %+v, want a plain approval only when resolved", outcome)
				}
			default:
				if tc.want {
					t.Error("resolved approval carries no outcome")
				}
			}
		})
	}
}

func flip(last byte) string {
	if last == 'a' {
		return "b"
	}
	return "a"
}

func TestResolveDecidesOnce(t *testing.T) {
	registry := newTestRegistry(t)
	binding := ownerBinding()
	expiresAt := time.Now().Add(time.Minute)
	tokenID, pending, err := registry.Issue(binding, expiresAt)
	if err != nil {
		t.Fatalf("Issue(): %v", err)
	}
	callbacks := registry.Callbacks(tokenID, binding, expiresAt)
	presser := &Presser{Actor: "111", Scope: [2]string{"222", "333"}}
	if !registry.Resolve(callbacks.Reject, presser) {
		t.Fatal("first resolve was refused")
	}
	if registry.Resolve(callbacks.Approve, presser) || registry.Resolve(callbacks.Reject, presser) {
		t.Error("a decided approval resolved again")
	}
	if outcome := <-pending.result; outcome.Accepted {
		t.Errorf("outcome = %+v, want the first decision to stand", outcome)
	}
}

func TestResolveStandingNeedsTheOffer(t *testing.T) {
	registry := newTestRegistry(t)
	binding := ownerBinding()
	expiresAt := time.Now().Add(time.Minute)
	tokenID, _, err := registry.Issue(binding, expiresAt)
	if err != nil {
		t.Fatalf("Issue(): %v", err)
	}
	forged := CallbackData(tokenID, ActionStanding, registry.mac(tokenID, ActionStanding, binding, expiresAt.Unix()))
	if registry.Resolve(forged, &Presser{Actor: "111", Scope: [2]string{"222", "333"}}) {
		t.Error("standing action resolved without the offer")
	}
}

func TestResolveRefusesExpiredApprovals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newTestRegistry(t)
		binding := ownerBinding()
		expiresAt := time.Now().Add(time.Minute)
		tokenID, _, err := registry.Issue(binding, expiresAt)
		if err != nil {
			t.Fatalf("Issue(): %v", err)
		}
		data := registry.Callbacks(tokenID, binding, expiresAt).Approve
		time.Sleep(2 * time.Minute)
		if registry.Resolve(data, &Presser{Actor: "111", Scope: [2]string{"222", "333"}}) {
			t.Error("expired approval resolved")
		}
		if registry.Len() != 0 {
			t.Errorf("pending approvals = %d, want the expired one removed", registry.Len())
		}
	})
}

func TestResolveRefusesUnknownTokens(t *testing.T) {
	registry := newTestRegistry(t)
	if registry.Resolve("aura:0123456789abcdef:a:0123456789abcdef", &Presser{Actor: "111"}) {
		t.Error("unknown token resolved")
	}
}

func TestNilArgumentsFailClosed(t *testing.T) {
	registry := newTestRegistry(t)
	if registry.Resolve("aura:0123456789abcdef:a:0123456789abcdef", nil) {
		t.Error("nil presser resolved")
	}
	var missing *Registry
	if missing.Resolve("aura:0123456789abcdef:a:0123456789abcdef", &Presser{}) {
		t.Error("nil registry resolved")
	}
	if _, _, err := registry.Issue(nil, time.Now()); err == nil {
		t.Error("nil binding issued")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Errorf("Issue(nil) code = %q, want %q", code, ErrorCodeInvalidArgument)
	}
	if _, _, err := registry.Await(t.Context(), "token", nil); err == nil {
		t.Error("nil pending awaited")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
		t.Errorf("Await(nil) code = %q, want %q", code, ErrorCodeInvalidArgument)
	}
}

func TestRegistryStaysBounded(t *testing.T) {
	registry := newTestRegistry(t)
	for range MaxPending + 50 {
		if _, _, err := registry.Issue(ownerBinding(), time.Now().Add(time.Minute)); err != nil {
			t.Fatalf("Issue(): %v", err)
		}
	}
	if got := registry.Len(); got > MaxPending {
		t.Errorf("pending approvals = %d, want at most %d", got, MaxPending)
	}
	for i := range MaxSeen + 50 {
		registry.NoteSeen(strings.Repeat("x", i%7) + string(rune('a'+i%26)) + time.Duration(i).String())
	}
	registry.mu.Lock()
	seen := len(registry.seen)
	registry.mu.Unlock()
	if seen > MaxSeen {
		t.Errorf("seen ids = %d, want at most %d", seen, MaxSeen)
	}
}

func TestNoteSeenReportsDuplicates(t *testing.T) {
	registry := newTestRegistry(t)
	if !registry.NoteSeen("cb-1") {
		t.Error("first sighting reported as duplicate")
	}
	if registry.NoteSeen("cb-1") {
		t.Error("second sighting reported as new")
	}
}

func TestAwait(t *testing.T) {
	t.Run("decision", func(t *testing.T) {
		registry := newTestRegistry(t)
		binding := ownerBinding()
		expiresAt := time.Now().Add(time.Minute)
		tokenID, pending, err := registry.Issue(binding, expiresAt)
		if err != nil {
			t.Fatalf("Issue(): %v", err)
		}
		callbacks := registry.Callbacks(tokenID, binding, expiresAt)
		registry.Resolve(callbacks.Approve, &Presser{Actor: "111", Scope: [2]string{"222", "333"}})
		outcome, timedOut, err := registry.Await(t.Context(), tokenID, pending)
		if err != nil || timedOut || !outcome.Accepted {
			t.Errorf("Await() = %+v, %v, %v; want accepted", outcome, timedOut, err)
		}
		if registry.Len() != 0 {
			t.Error("pending approval kept after the decision")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			registry := newTestRegistry(t)
			tokenID, pending, err := registry.Issue(ownerBinding(), time.Now().Add(time.Minute))
			if err != nil {
				t.Fatalf("Issue(): %v", err)
			}
			outcome, timedOut, err := registry.Await(t.Context(), tokenID, pending)
			if err != nil || !timedOut || outcome.Accepted {
				t.Errorf("Await() = %+v, %v, %v; want timeout", outcome, timedOut, err)
			}
			if registry.Len() != 0 {
				t.Error("pending approval kept after the timeout")
			}
		})
	})
	t.Run("cancel", func(t *testing.T) {
		registry := newTestRegistry(t)
		tokenID, pending, err := registry.Issue(ownerBinding(), time.Now().Add(time.Minute))
		if err != nil {
			t.Fatalf("Issue(): %v", err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		outcome, timedOut, err := registry.Await(ctx, tokenID, pending)
		if !errors.Is(err, context.Canceled) || timedOut || outcome.Accepted {
			t.Errorf("Await() = %+v, %v, %v; want context.Canceled", outcome, timedOut, err)
		}
		if registry.Len() != 0 {
			t.Error("pending approval kept after the cancel")
		}
	})
}

func TestStandingGrantLabelFollowsTheTTL(t *testing.T) {
	for ttl, want := range map[time.Duration]string{
		168 * time.Hour:  "Allow 7 days",
		24 * time.Hour:   "Allow 1 day",
		36 * time.Hour:   "Allow 36 hours",
		90 * time.Minute: "Allow 1h30m0s",
	} {
		if got := StandingGrantLabel(ttl); got != want {
			t.Errorf("StandingGrantLabel(%v) = %q, want %q", ttl, got, want)
		}
	}
}

type recorder struct{}

func (recorder) RecordStandingGrant(context.Context, *StandingGrantRequest) error { return nil }

func TestOffersStandingGrant(t *testing.T) {
	cases := []struct {
		name   string
		prompt *Prompt
		class  principal.Class
		grants StandingGrantRecorder
		ttl    time.Duration
		want   bool
	}{
		{"trusted with store and ttl", &Prompt{}, principal.ClassTrusted, recorder{}, time.Hour, true},
		{"owner", &Prompt{}, principal.ClassOwner, recorder{}, time.Hour, false},
		{"public", &Prompt{}, principal.ClassPublic, recorder{}, time.Hour, false},
		{"no store", &Prompt{}, principal.ClassTrusted, nil, time.Hour, false},
		{"no ttl", &Prompt{}, principal.ClassTrusted, recorder{}, 0, false},
		{"untrusted input", &Prompt{ReasonCode: policy.ReasonUntrustedInput}, principal.ClassTrusted, recorder{}, time.Hour, false},
		{"nil prompt", nil, principal.ClassTrusted, recorder{}, time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OffersStandingGrant(tc.prompt, tc.class, StandingOffer{Grants: tc.grants, TTL: tc.ttl}); got != tc.want {
				t.Errorf("OffersStandingGrant() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOwnerTimeoutPrefersTheMostSpecificValue(t *testing.T) {
	cases := []struct {
		name                string
		persona, configured time.Duration
		want                time.Duration
	}{
		{"persona", time.Minute, time.Hour, time.Minute},
		{"configured", 0, time.Hour, time.Hour},
		{"fallback", 0, 0, FallbackTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OwnerTimeout(tc.persona, tc.configured); got != tc.want {
				t.Errorf("OwnerTimeout() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPersonaLabel(t *testing.T) {
	if got := PersonaLabel("team", "Team"); got != "Team (team)" {
		t.Errorf("PersonaLabel() = %q", got)
	}
	if got := PersonaLabel("team", ""); got != "team" {
		t.Errorf("PersonaLabel() = %q", got)
	}
	if got := PersonaLabel("team", "team"); got != "team" {
		t.Errorf("PersonaLabel() = %q", got)
	}
}

func TestRenderCardCleansArgumentsAndNeverTruncatesThem(t *testing.T) {
	tail := strings.Repeat("x", 3000)
	prompt := &Prompt{ToolName: "shell.exec", ToolVersion: "1", SessionID: "s", PrincipalID: "owner", Arguments: "SECRET " + tail}
	card := RenderCard(prompt, "Team (team)", func(text string) string { return strings.ReplaceAll(text, "SECRET", "[clean]") })
	if strings.Contains(card, "SECRET") || !strings.Contains(card, "[clean]") {
		t.Error("card skipped the cleaner")
	}
	if !strings.Contains(card, tail) || strings.Contains(card, "truncated") || !strings.Contains(card, "persona: Team (team)") {
		t.Errorf("card cut the arguments: %d bytes", len(card))
	}
	if RenderCard(nil, "", nil) != "" {
		t.Error("nil prompt rendered a card")
	}
	empty := RenderCard(&Prompt{ToolName: "t", SessionID: "s", PrincipalID: "p"}, "", nil)
	if !strings.Contains(empty, "arguments: {}") {
		t.Errorf("empty arguments card = %q", empty)
	}
}

func TestCardStatesTheUntrustedInputReason(t *testing.T) {
	arguments := strings.Repeat("x", 1200)
	for _, reason := range []string{"", "approval_required", policy.ReasonUntrustedInput} {
		t.Run("reason "+reason, func(t *testing.T) {
			card := RenderCard(&Prompt{ToolName: "web_fetch", ToolVersion: "v1", SessionID: "s", PrincipalID: "p", Arguments: arguments, ReasonCode: reason}, "team", nil)
			lines := strings.Split(card, "\n")
			count := strings.Count(card, policy.UntrustedInputNotice)
			if reason != policy.ReasonUntrustedInput {
				if count != 0 {
					t.Errorf("card states the untrusted input reason for %q", reason)
				}
				return
			}
			if count != 1 || !strings.HasPrefix(lines[0], "approval required: web_fetch") || lines[1] != policy.UntrustedInputNotice {
				t.Errorf("card does not state the reason as its second line, once: %q", lines[:3])
			}
		})
	}
}

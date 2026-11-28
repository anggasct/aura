package approval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	policy "github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
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
		{"bad action", "aura:0123456789abcdef:z:0123456789abcdef", false},
		{"non-hex mac", "aura:0123456789abcdef:a:zzzzzzzzzzzzzzzz", false},
		{"uppercase hex", "aura:0123456789ABCDEF:a:0123456789abcdef", false},
		{"extra part", "aura:0123456789abcdef:a:0123456789abcdef:x", false},
		{"approve", "aura:0123456789abcdef:a:0123456789abcdef", true},
		{"reject", "aura:0123456789abcdef:r:fedcba9876543210", true},
		{"standing", "aura:0123456789abcdef:s:fedcba9876543210", true},
		{"stop", "aura:0123456789abcdef:x:fedcba9876543210", true},
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
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeInvalidArgument {
		t.Errorf("Issue(nil) code = %q, want %q", code, ErrorCodeInvalidArgument)
	}
	if _, _, err := registry.Await(t.Context(), "token", nil); err == nil {
		t.Error("nil pending awaited")
	} else if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeInvalidArgument {
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

func builtPersona(t *testing.T, lang, name string, overrides map[string]string) auraagent.Definition {
	t.Helper()
	persona := auraagent.Persona{
		ID:       auraagent.DefaultID,
		Name:     name,
		Messages: overrides,
	}
	registry, err := auraagent.Build([]auraagent.Persona{persona}, []string{"read_file", "write_file", "list_dir", "exec", "web_fetch", "web_search", "aura_operation_search"}, nil, []string{"primary"}, lang)
	if err != nil {
		t.Fatalf("agent.Build(%q): %v", lang, err)
	}
	def, ok := registry.Lookup(auraagent.DefaultID)
	if !ok {
		t.Fatal("default persona missing from registry")
	}
	return def
}

func TestFormatDurationAndButtonsFollowLocale(t *testing.T) {
	en := builtPersona(t, "en", "Aura", nil)
	id := builtPersona(t, "id", "Aura", nil)
	for _, tc := range []struct {
		name    string
		persona auraagent.Definition
		ttl     time.Duration
		wantDur string
		wantBtn CardButtons
	}{
		{
			name:    "en 1 day",
			persona: en,
			ttl:     24 * time.Hour,
			wantDur: "1 day",
			wantBtn: CardButtons{Approve: "Approve", Reject: "Reject", Standing: "Allow for 1 day"},
		},
		{
			name:    "en 7 days",
			persona: en,
			ttl:     168 * time.Hour,
			wantDur: "7 days",
			wantBtn: CardButtons{Approve: "Approve", Reject: "Reject", Standing: "Allow for 7 days"},
		},
		{
			name:    "en 12 hours",
			persona: en,
			ttl:     12 * time.Hour,
			wantDur: "12 hours",
			wantBtn: CardButtons{Approve: "Approve", Reject: "Reject", Standing: "Allow for 12 hours"},
		},
		{
			name:    "en fractional fallback",
			persona: en,
			ttl:     90 * time.Minute,
			wantDur: "1h30m0s",
			wantBtn: CardButtons{Approve: "Approve", Reject: "Reject", Standing: "Allow for 1h30m0s"},
		},
		{
			name:    "id 1 day",
			persona: id,
			ttl:     24 * time.Hour,
			wantDur: "1 hari",
			wantBtn: CardButtons{Approve: "Izinkan", Reject: "Tolak", Standing: "Izinkan 1 hari"},
		},
		{
			name:    "id 7 days",
			persona: id,
			ttl:     168 * time.Hour,
			wantDur: "7 hari",
			wantBtn: CardButtons{Approve: "Izinkan", Reject: "Tolak", Standing: "Izinkan 7 hari"},
		},
		{
			name:    "id 12 hours",
			persona: id,
			ttl:     12 * time.Hour,
			wantDur: "12 jam",
			wantBtn: CardButtons{Approve: "Izinkan", Reject: "Tolak", Standing: "Izinkan 12 jam"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatDuration(&tc.persona, tc.ttl); got != tc.wantDur {
				t.Errorf("FormatDuration(%v) = %q, want %q", tc.ttl, got, tc.wantDur)
			}
			if got := RenderButtons(&tc.persona, tc.ttl); got != tc.wantBtn {
				t.Errorf("RenderButtons(%v) = %+v, want %+v", tc.ttl, got, tc.wantBtn)
			}
		})
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
		{"trusted with store and ttl", &Prompt{StandingGrantAllowed: true}, principal.ClassTrusted, recorder{}, time.Hour, true},
		{"owner", &Prompt{StandingGrantAllowed: true}, principal.ClassOwner, recorder{}, time.Hour, false},
		{"public", &Prompt{StandingGrantAllowed: true}, principal.ClassPublic, recorder{}, time.Hour, false},
		{"no store", &Prompt{StandingGrantAllowed: true}, principal.ClassTrusted, nil, time.Hour, false},
		{"no ttl", &Prompt{StandingGrantAllowed: true}, principal.ClassTrusted, recorder{}, 0, false},
		{"untrusted input", &Prompt{ReasonCode: policy.ReasonUntrustedInput, StandingGrantAllowed: true}, principal.ClassTrusted, recorder{}, time.Hour, false},
		{"untrusted trust grant", &Prompt{ReasonCode: policy.ReasonUntrustedTrustGrant, StandingGrantAllowed: true}, principal.ClassTrusted, recorder{}, time.Hour, false},
		{"per call only", &Prompt{}, principal.ClassTrusted, recorder{}, time.Hour, false},
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

func TestCardTextIsHumanAndExact(t *testing.T) {
	en := builtPersona(t, "en", "Aura", nil)
	id := builtPersona(t, "id", "Aura", nil)
	cutArgs := `{"command":"` + strings.Repeat("a", 40) + `"}...[truncated]`
	cases := []struct {
		name           string
		prompt         Prompt
		inlineCodeTool bool
		wantENHeader   string
		wantIDHeader   string
		wantDetails    string
	}{
		{
			name: "exec",
			prompt: Prompt{
				ToolName:    "exec",
				ToolVersion: "v1",
				SessionID:   "main/telegram:pocket/chat:1",
				PrincipalID: principal.OwnerID,
				Arguments:   `{"command":"git status"}`,
			},
			wantENHeader: "Aura wants to run a command:",
			wantIDHeader: "Aura mau menjalankan perintah:",
			wantDetails:  "exec@v1\n" + `{"command":"git status"}`,
		},
		{
			name: "write_file",
			prompt: Prompt{
				ToolName:    "write_file",
				ToolVersion: "v1",
				SessionID:   "main/telegram:pocket/chat:1",
				PrincipalID: principal.OwnerID,
				Arguments:   `{"path":"notes.md","content":"hi"}`,
			},
			wantENHeader: "Aura wants to write a file:",
			wantIDHeader: "Aura mau menulis file:",
			wantDetails:  "write_file@v1\n" + `{"path":"notes.md","content":"hi"}`,
		},
		{
			name: "github tool",
			prompt: Prompt{
				ToolName:    "github_create_pr",
				ToolVersion: "v1",
				SessionID:   "main/telegram:pocket/chat:1",
				PrincipalID: principal.OwnerID,
				Arguments:   "repo: owner/app\ntitle: Fix bug",
			},
			wantENHeader: "Aura wants to open a pull request:",
			wantIDHeader: "Aura mau membuat pull request:",
			wantDetails:  "github_create_pr@v1\nrepo: owner/app\ntitle: Fix bug",
		},
		{
			name: "mcp tool generic plain",
			prompt: Prompt{
				ToolName:    "mcp_linear_create_issue",
				ToolVersion: "v1",
				SessionID:   "main/telegram:pocket/chat:1",
				PrincipalID: principal.OwnerID,
				Arguments:   `{"title":"Bug"}`,
			},
			wantENHeader: "Aura wants to use mcp_linear_create_issue:",
			wantIDHeader: "Aura mau memakai mcp_linear_create_issue:",
			wantDetails:  "mcp_linear_create_issue@v1\n" + `{"title":"Bug"}`,
		},
		{
			name: "mcp tool generic inline code",
			prompt: Prompt{
				ToolName:    "mcp_linear_create_issue",
				ToolVersion: "v1",
				SessionID:   "main/discord:home/dm:1",
				PrincipalID: principal.OwnerID,
				Arguments:   `{"title":"Bug"}`,
			},
			inlineCodeTool: true,
			wantENHeader:   "Aura wants to use `mcp_linear_create_issue`:",
			wantIDHeader:   "Aura mau memakai `mcp_linear_create_issue`:",
			wantDetails:    "mcp_linear_create_issue@v1\n" + `{"title":"Bug"}`,
		},
		{
			name: "guest request",
			prompt: Prompt{
				ToolName:    "email_send",
				ToolVersion: "v1",
				SessionID:   "main/telegram:pocket/chat:99",
				PrincipalID: "telegram:555001",
				Arguments:   "to: bob@example.com",
			},
			wantENHeader: "Aura wants to send an email:\nAsked by telegram:555001",
			wantIDHeader: "Aura mau mengirim email:\nDiminta oleh telegram:555001",
			wantDetails:  "email_send@v1\nto: bob@example.com",
		},
		{
			name: "workflow call card",
			prompt: Prompt{
				ToolName:    "code_execute",
				ToolVersion: "v1",
				SessionID:   "wf-run-42",
				PrincipalID: "discord:111",
				Arguments:   `{"runtime":"python","code":"print(1)"}`,
			},
			wantENHeader: "Aura wants to run code:\nAsked by discord:111\nWorkflow run run-42",
			wantIDHeader: "Aura mau menjalankan kode:\nDiminta oleh discord:111\nWorkflow run run-42",
			wantDetails:  "code_execute@v1\n" + `{"runtime":"python","code":"print(1)"}`,
		},
		{
			name: "untrusted input",
			prompt: Prompt{
				ToolName:    "github_comment",
				ToolVersion: "v1",
				SessionID:   "main/telegram:pocket/chat:1",
				PrincipalID: principal.OwnerID,
				Arguments:   "body: hello",
				ReasonCode:  policy.ReasonUntrustedInput,
			},
			wantENHeader: "Aura wants to post a comment on GitHub:\n" + policy.UntrustedInputNotice,
			wantIDHeader: "Aura mau mengirim komentar di GitHub:\ninput tidak tepercaya: isi dari luar kendalimu membentuk permintaan ini, jadi hanya bisa disetujui sekali",
			wantDetails:  "github_comment@v1\nbody: hello",
		},
		{
			name: "cut display",
			prompt: Prompt{
				ToolName:    "github_merge",
				ToolVersion: "v1",
				SessionID:   "main/telegram:pocket/chat:1",
				PrincipalID: principal.OwnerID,
				Arguments:   "  " + cutArgs + "  ",
			},
			wantENHeader: "Aura wants to merge a pull request:",
			wantIDHeader: "Aura mau menggabungkan pull request:",
			wantDetails:  "github_merge@v1\n" + cutArgs,
		},
	}

	for _, tc := range cases {
		for _, lang := range []struct {
			code       string
			persona    auraagent.Definition
			wantHeader string
		}{
			{"en", en, tc.wantENHeader},
			{"id", id, tc.wantIDHeader},
		} {
			t.Run(tc.name+"/"+lang.code, func(t *testing.T) {
				got := RenderCard(&tc.prompt, CardOptions{Persona: &lang.persona, InlineCodeTool: tc.inlineCodeTool})
				if got.Header != lang.wantHeader {
					t.Errorf("Header =\n%q\nwant:\n%q", got.Header, lang.wantHeader)
				}
				if got.Details != tc.wantDetails {
					t.Errorf("Details =\n%q\nwant:\n%q", got.Details, tc.wantDetails)
				}
				if got.Text != lang.wantHeader+"\n"+tc.wantDetails {
					t.Errorf("Text =\n%q\nwant:\n%q", got.Text, lang.wantHeader+"\n"+tc.wantDetails)
				}
				if strings.Contains(got.Text, "session:") || strings.Contains(got.Text, "approve exactly this request?") {
					t.Errorf("card still carries legacy lines: %q", got.Text)
				}
				if tc.name == "cut display" && !strings.HasSuffix(got.Details, "...[truncated]") {
					t.Errorf("cut details lost truncation suffix: %q", got.Details)
				}
			})
		}
	}
}

func TestRenderCardCleansArgumentsAndNeverTruncatesThem(t *testing.T) {
	tail := strings.Repeat("x", 3000)
	prompt := &Prompt{ToolName: "shell.exec", ToolVersion: "1", SessionID: "s", PrincipalID: principal.OwnerID, Arguments: "SECRET " + tail}
	card := RenderCard(prompt, CardOptions{
		FallbackName: "Team",
		Clean:        func(text string) string { return strings.ReplaceAll(text, "SECRET", "[clean]") },
	})
	if strings.Contains(card.Text, "SECRET") || !strings.Contains(card.Text, "[clean]") {
		t.Error("card skipped the cleaner")
	}
	if !strings.Contains(card.Details, tail) || strings.Contains(card.Text, "truncated") || !strings.HasPrefix(card.Header, "Team wants to use shell.exec:") {
		t.Errorf("card cut the arguments: %d bytes", len(card.Text))
	}
	if RenderCard(nil, CardOptions{}).Text != "" {
		t.Error("nil prompt rendered a card")
	}
	empty := RenderCard(&Prompt{ToolName: "t", SessionID: "s", PrincipalID: principal.OwnerID}, CardOptions{})
	if empty.Details != "t\n{}" {
		t.Errorf("empty arguments details = %q, want %q", empty.Details, "t\n{}")
	}
}

func TestCardStatesTheUntrustedInputReason(t *testing.T) {
	arguments := strings.Repeat("x", 1200)
	for _, reason := range []string{"", "approval_required", policy.ReasonUntrustedInput} {
		t.Run("reason "+reason, func(t *testing.T) {
			card := RenderCard(&Prompt{ToolName: "web_fetch", ToolVersion: "v1", SessionID: "s", PrincipalID: principal.OwnerID, Arguments: arguments, ReasonCode: reason}, CardOptions{FallbackName: "Team"}).Text
			lines := strings.Split(card, "\n")
			count := strings.Count(card, policy.UntrustedInputNotice)
			if reason != policy.ReasonUntrustedInput {
				if count != 0 {
					t.Errorf("card states the untrusted input reason for %q", reason)
				}
				return
			}
			if count != 1 || lines[0] != "Team wants to use web_fetch:" || lines[1] != policy.UntrustedInputNotice {
				t.Errorf("card does not state the reason as its second line, once: %q", lines[:3])
			}
		})
	}
}

func TestCardStatesTheContentUnverifiedReason(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{reason: policy.ReasonContentUnverified, want: policy.ContentUnverifiedNotice},
		{reason: policy.ReasonUntrustedInput, want: policy.UntrustedInputNotice},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			card := RenderCard(&Prompt{ToolName: "aura_cron_add", ToolVersion: "v1", SessionID: "s", PrincipalID: principal.OwnerID, Arguments: "{}", ReasonCode: tc.reason}, CardOptions{FallbackName: "Team"}).Text
			lines := strings.Split(card, "\n")
			if lines[1] != tc.want {
				t.Errorf("second line = %q, want %q", lines[1], tc.want)
			}
			other := policy.ContentUnverifiedNotice
			if tc.reason == policy.ReasonContentUnverified {
				other = policy.UntrustedInputNotice
			}
			if strings.Contains(card, other) {
				t.Errorf("card carries the notice of the other reason: %q", card)
			}
		})
	}
}

func TestRenderCardEscapesEveryField(t *testing.T) {
	hostile := "\u202e\U000E0041"
	persona := auraagent.Definition{ID: "main", Name: "Aura" + hostile}
	card := RenderCard(&Prompt{
		ToolName:    "t" + hostile,
		ToolVersion: "v" + hostile,
		SessionID:   "wf-run" + hostile,
		PrincipalID: "p" + hostile,
		Arguments:   `a\\b`,
		TaintSource: "src" + hostile,
	}, CardOptions{Persona: &persona})
	for _, field := range []string{"Aura", "t", "v", "run", "p", "src"} {
		if !strings.Contains(card.Text, field+`\u{202e}\u{e0041}`) {
			t.Errorf("card does not escape the %q field: %q", field, card.Text)
		}
	}
	if strings.ContainsRune(card.Text, '\u202e') || strings.ContainsRune(card.Text, '\U000E0041') {
		t.Errorf("card holds a raw hostile rune: %q", card.Text)
	}
	if !strings.HasSuffix(card.Details, "\n"+`a\\b`) {
		t.Errorf("card changed the arguments: %q", card.Details)
	}
}

func TestCardOverridesKeepPlaceholdersAndBundleOnlyKeys(t *testing.T) {
	t.Run("override without persona placeholder fails persona_invalid", func(t *testing.T) {
		cfg := auraagent.Config{
			Definitions: []auraagent.Persona{{
				ID:           "kantor",
				Description:  "d",
				Instructions: "i",
				Messages:     map[string]string{auraagent.MessageApprovalSummary: "Wants to {action}:"},
			}},
		}
		err := cfg.Validate()
		if !fault.HasCode(err, auraagent.ErrorCodePersonaInvalid) {
			t.Fatalf("Validate() err = %v, want %s", err, auraagent.ErrorCodePersonaInvalid)
		}
	})
	t.Run("bundle-only keys fail persona_invalid", func(t *testing.T) {
		for _, key := range []string{auraagent.MessageApprovalActionExec, auraagent.MessageApprovalUntrustedInput} {
			cfg := auraagent.Config{
				Definitions: []auraagent.Persona{{
					ID:           "kantor",
					Description:  "d",
					Instructions: "i",
					Messages:     map[string]string{key: "override"},
				}},
			}
			err := cfg.Validate()
			if !fault.HasCode(err, auraagent.ErrorCodePersonaInvalid) {
				t.Fatalf("Validate(%s) err = %v, want %s", key, err, auraagent.ErrorCodePersonaInvalid)
			}
		}
	})
	t.Run("persona name with invisible character is escaped", func(t *testing.T) {
		persona := auraagent.Definition{ID: "main", Name: "Aura\u202e\U000E0041"}
		card := RenderCard(&Prompt{
			ToolName:    "exec",
			ToolVersion: "v1",
			SessionID:   "s",
			PrincipalID: principal.OwnerID,
			Arguments:   `{}`,
		}, CardOptions{Persona: &persona})
		if !strings.Contains(card.Header, `Aura\u{202e}\u{e0041}`) {
			t.Fatalf("Header = %q, want escaped invisible runes", card.Header)
		}
		if strings.ContainsRune(card.Header, '\u202e') || strings.ContainsRune(card.Header, '\U000E0041') {
			t.Fatalf("Header kept raw invisible rune: %q", card.Header)
		}
	})
}

func TestCardSubstitutesInOnePassWithoutDoubleExpanding(t *testing.T) {
	persona := auraagent.Definition{ID: "main", Name: "{action}"}
	card := RenderCard(&Prompt{
		ToolName:    "exec",
		ToolVersion: "v1",
		SessionID:   "s",
		PrincipalID: principal.OwnerID,
		Arguments:   `{}`,
	}, CardOptions{Persona: &persona})
	if card.Header != "{action} wants to run a command:" {
		t.Errorf("Header = %q, want single-pass substitution", card.Header)
	}
}

func TestResolveActionHandlesStopTokensAndKeepsResolveScopedToApprovals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := newTestRegistry(t)
		expiresAt := time.Now().Add(time.Minute)
		binding := &Binding{
			Class:     principal.ClassOwner,
			Actor:     "42",
			Scope:     [2]string{"guild-1", "thread-9"},
			SessionID: "main/discord:bot/thread:thread-9",
			TurnID:    "turn-77",
		}
		tokenID, _, err := registry.Issue(binding, expiresAt)
		if err != nil {
			t.Fatalf("Issue(): %v", err)
		}
		data := registry.Callback(tokenID, ActionStop, binding, expiresAt)
		if _, action, _, ok := ParseCallbackData(data); !ok || action != ActionStop {
			t.Fatalf("ParseCallbackData(%q) = %q, %v", data, action, ok)
		}
		presser := &Presser{Actor: "42", Scope: [2]string{"guild-1", "thread-9"}}
		if registry.Resolve(data, presser) {
			t.Fatal("Resolve accepted a stop token")
		}
		if _, _, ok := registry.ResolveAction(data, &Presser{Actor: "99", Scope: presser.Scope}); ok {
			t.Fatal("ResolveAction accepted another actor")
		}
		if _, _, ok := registry.ResolveAction(data, &Presser{Actor: "42", Scope: [2]string{"", "thread-9"}}); ok {
			t.Fatal("ResolveAction accepted another guild scope")
		}
		action, got, ok := registry.ResolveAction(data, presser)
		if !ok || action != ActionStop || got.SessionID != binding.SessionID || got.TurnID != binding.TurnID {
			t.Fatalf("ResolveAction() = %q, %+v, %v", action, got, ok)
		}
		if registry.Len() != 0 {
			t.Fatalf("pending stop tokens = %d after resolve, want 0", registry.Len())
		}
		if _, _, ok := registry.ResolveAction(data, presser); ok {
			t.Fatal("ResolveAction resolved a stop token twice")
		}
	})
}

func TestCardStatesTheUntrustedTrustGrantWarningFirst(t *testing.T) {
	card := RenderCard(&Prompt{
		ToolName:    "aura_cron_add",
		ToolVersion: "v1",
		SessionID:   "s",
		PrincipalID: principal.OwnerID,
		Arguments:   "name: nightly",
		ReasonCode:  policy.ReasonUntrustedTrustGrant,
		TaintSource: "a web_fetch result from example.com",
	}, CardOptions{FallbackName: "Main"}).Text
	lines := strings.Split(card, "\n")
	if len(lines) < 4 {
		t.Fatalf("card has too few lines: %q", card)
	}
	if lines[0] != "Main wants to use aura_cron_add:" {
		t.Errorf("line 0 = %q", lines[0])
	}
	if lines[1] != policy.UntrustedTrustGrantNotice {
		t.Errorf("line 1 = %q, want %q", lines[1], policy.UntrustedTrustGrantNotice)
	}
	if lines[2] != "came in through: a web_fetch result from example.com" {
		t.Errorf("line 2 = %q, want came in through line", lines[2])
	}
	if strings.Contains(card, policy.UntrustedInputNotice) {
		t.Errorf("card also includes the plain untrusted_input notice: %q", card)
	}
}

func TestCardNamesTheTaintSource(t *testing.T) {
	for _, reason := range []string{policy.ReasonUntrustedInput, policy.ReasonUntrustedTrustGrant} {
		t.Run(reason, func(t *testing.T) {
			card := RenderCard(&Prompt{
				ToolName:    "web_fetch",
				ToolVersion: "v1",
				SessionID:   "s",
				PrincipalID: principal.OwnerID,
				Arguments:   `{"url":"https://example.com"}`,
				ReasonCode:  reason,
				TaintSource: "a web_fetch result from xn--bcher-kva.example",
			}, CardOptions{FallbackName: "Main"}).Text
			lines := strings.Split(card, "\n")
			if len(lines) < 3 || lines[2] != "came in through: a web_fetch result from xn--bcher-kva.example" {
				t.Errorf("lines = %q, want came in through line right after notice", lines[:min(len(lines), 4)])
			}
		})
	}
}

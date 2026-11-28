package discord

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

func newLocalizedCardAdapter(t *testing.T, stub *approvalStub, lang string, ttl time.Duration, loc *time.Location) *Adapter {
	t.Helper()
	t.Setenv("AURA_DISCORD_BOT_TOKEN", testToken)
	cfg := testAdapterConfig()
	cfg.Accounts[0].Persona = "team"
	registry, err := auraagent.Build(accessPersonas(), accessTestTools, accessTestToolCatalog, []string{"primary"}, lang)
	if err != nil {
		t.Fatalf("agent.Build(%q): %v", lang, err)
	}
	router, err := auraagent.NewRouter(registry, discordChannelAccounts(&cfg), nil)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	observer := &accessObserver{denied: map[string]int{}, routed: map[string]int{}}
	settings := observer.settings()
	if ttl > 0 {
		settings.StandingGrantTTL = ttl
	}
	adapter, err := New(&Deps{
		Config:            &cfg,
		Account:           &cfg.Accounts[0],
		Resumes:           &memoryResumeStore{},
		Effects:           newFakeEffectRunner(),
		Media:             &fakeMediaStore{},
		Sessions:          &fakeSessionEnsurer{},
		Logger:            slog.New(slog.DiscardHandler),
		PersonaRouter:     router,
		PrincipalResolver: testOwners(t, "discord:777"),
		Access:            &settings,
		OwnerLocation:     loc,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	adapter.restBase = stub.srv.URL
	adapter.rememberTarget("dm:777", &approvalBinding{
		principal: principal.OwnerID,
		class:     principal.ClassOwner,
		persona:   "team",
		actor:     "777",
		channelID: "333",
	})
	return adapter
}

func TestDiscordCardFencesTheDetails(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newLocalizedCardAdapter(t, stub, "en", 7*24*time.Hour, nil)
	fence := strings.Repeat("`", 3)
	prompt := testPrompt("dm:777", time.Now().Add(time.Minute))
	prompt.ToolName = "mcp_github_issue"
	prompt.ToolVersion = "v2"
	prompt.Arguments = "note: @everyone " + fence + "boom"

	ctx, cancel := context.WithCancel(t.Context())
	result := decideAsync(ctx, adapter, prompt)
	post := stub.waitPost(t, 5*time.Second)
	cancel()
	awaitDecide(t, result, 5*time.Second)

	content, _ := post.body["content"].(string)
	want := "Team wants to use `mcp_github_issue`:\n" + fence + "\nmcp_github_issue@v2\nnote: @\u200beveryone ``\u200b`boom\n" + fence
	if content != want {
		t.Fatalf("card content =\n%q\nwant:\n%q", content, want)
	}
	if strings.Contains(content, "@everyone") {
		t.Errorf("card kept raw mention: %q", content)
	}
	if strings.Count(content, fence) != 2 {
		t.Errorf("card triple-backtick count = %d, want 2: %q", strings.Count(content, fence), content)
	}
}

func TestCardButtonsAreLocalized(t *testing.T) {
	cases := []struct {
		name       string
		lang       string
		ttl        time.Duration
		wantLabels []string
	}{
		{"en 1 day", "en", 24 * time.Hour, []string{"Approve", "Allow for 1 day", "Reject"}},
		{"en 7 days", "en", 7 * 24 * time.Hour, []string{"Approve", "Allow for 7 days", "Reject"}},
		{"en 12 hours", "en", 12 * time.Hour, []string{"Approve", "Allow for 12 hours", "Reject"}},
		{"id 1 day", "id", 24 * time.Hour, []string{"Izinkan", "Izinkan 1 hari", "Tolak"}},
		{"id 7 days", "id", 7 * 24 * time.Hour, []string{"Izinkan", "Izinkan 7 hari", "Tolak"}},
		{"id 12 hours", "id", 12 * time.Hour, []string{"Izinkan", "Izinkan 12 jam", "Tolak"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newApprovalStub(t)
			adapter := newLocalizedCardAdapter(t, stub, tc.lang, tc.ttl, nil)
			result := decideForOwnerAsync(t.Context(), adapter, httpPrompt(time.Now().Add(time.Minute)), httpRequester(principal.ClassTrusted))
			card := stub.waitPostTo(t, "555", 1)[0]
			labels, ids := cardButtons(t, card)
			if !slices.Equal(labels, tc.wantLabels) {
				t.Fatalf("button labels = %v, want %v", labels, tc.wantLabels)
			}
			tokenA, actA, _, okA := approval.ParseCallbackData(ids[0])
			tokenS, actS, _, okS := approval.ParseCallbackData(ids[1])
			tokenR, actR, _, okR := approval.ParseCallbackData(ids[2])
			if !okA || !okS || !okR || actA != approval.ActionApprove || actS != approval.ActionStanding || actR != approval.ActionReject {
				t.Fatalf("custom_ids invalid: %v", ids)
			}
			if tokenA != tokenS || tokenA != tokenR {
				t.Fatalf("custom_id tokens differ: %q %q %q", tokenA, tokenS, tokenR)
			}
			adapter.resolveComponentClick(t.Context(), clickPayload("ix-1", "777", "", "555", ids[0]))
			if outcome := awaitDecide(t, result, 5*time.Second); outcome.err != nil || !outcome.accepted {
				t.Fatalf("DecideForOwner() = %+v, want accepted", outcome)
			}
		})
	}
}

func TestCardIsEditedWithTheOutcome(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	fixedAt := time.Date(2026, 11, 28, 14, 5, 0, 0, time.UTC)

	cases := []struct {
		name     string
		lang     string
		loc      *time.Location
		action   string
		wantLine string
	}{
		{"approve in owner timezone", "en", jakarta, "approve", "✅ Approved · 21:05"},
		{"approve in UTC without timezone", "en", nil, "approve", "✅ Approved · 14:05 UTC"},
		{"standing in id timezone", "id", jakarta, "standing", "✅ Diizinkan 7 hari · 21:05"},
		{"reject in owner timezone", "en", jakarta, "reject", "❌ Rejected · 21:05"},
		{"expiry in id without timezone", "id", nil, "expiry", "⌛ Kedaluwarsa · 14:05 UTC"},
		{"cancel in owner timezone", "en", jakarta, "cancel", "⏹ Cancelled · 21:05"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := newApprovalStub(t)
			adapter := newLocalizedCardAdapter(t, stub, tc.lang, 7*24*time.Hour, tc.loc)
			adapter.now = func() time.Time { return fixedAt }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			lifetime := time.Minute
			if tc.action == "expiry" {
				lifetime = 40 * time.Millisecond
			}
			prompt := httpPrompt(time.Now().Add(lifetime))
			prompt.Arguments = `{"path":"a.txt"}`
			result := decideForOwnerAsync(ctx, adapter, prompt, httpRequester(principal.ClassTrusted))
			card := stub.waitPostTo(t, "555", 1)[0]
			originalContent, _ := card.body["content"].(string)
			_, ids := cardButtons(t, card)

			switch tc.action {
			case "approve":
				adapter.resolveComponentClick(t.Context(), clickPayload("ix-1", "777", "", "555", ids[0]))
			case "standing":
				adapter.resolveComponentClick(t.Context(), clickPayload("ix-1", "777", "", "555", ids[1]))
			case "reject":
				adapter.resolveComponentClick(t.Context(), clickPayload("ix-1", "777", "", "555", ids[2]))
			case "cancel":
				cancel()
			}
			awaitDecide(t, result, 5*time.Second)

			stub.mu.Lock()
			patches := slices.Clone(stub.patches)
			stub.mu.Unlock()
			if len(patches) != 1 {
				t.Fatalf("patches = %d, want 1", len(patches))
			}
			gotContent, _ := patches[0].body["content"].(string)
			wantContent := originalContent + "\n" + tc.wantLine
			if gotContent != wantContent {
				t.Errorf("edited content =\n%q\nwant:\n%q", gotContent, wantContent)
			}
			if flags, _ := patches[0].body["flags"].(float64); flags != suppressEmbedsFlags {
				t.Errorf("edit flags = %v, want %d", patches[0].body["flags"], suppressEmbedsFlags)
			}
			allowedMentions, _ := patches[0].body["allowed_mentions"].(map[string]any)
			parse, ok := allowedMentions["parse"].([]any)
			if !ok || len(parse) != 0 {
				t.Errorf("edit allowed_mentions = %v, want parse: []", patches[0].body["allowed_mentions"])
			}
			components, ok := patches[0].body["components"].([]any)
			if !ok || len(components) != 0 {
				t.Errorf("edit components = %v, want empty slice", patches[0].body["components"])
			}
		})
	}

	t.Run("failing edit falls back to clearing components", func(t *testing.T) {
		stub := newApprovalStub(t)
		stub.patchCodes = []int{http.StatusInternalServerError, http.StatusOK}
		adapter := newLocalizedCardAdapter(t, stub, "en", 7*24*time.Hour, jakarta)
		adapter.now = func() time.Time { return fixedAt }

		result := decideAsync(t.Context(), adapter, testPrompt("dm:777", time.Now().Add(time.Minute)))
		approve, _ := cardCustomIDs(t, stub.waitPost(t, 5*time.Second))
		adapter.resolveComponentClick(t.Context(), clickPayload("ix-1", "777", "", "333", approve))
		outcome := awaitDecide(t, result, 5*time.Second)
		if outcome.err != nil || !outcome.accepted {
			t.Fatalf("Decide() = %+v, want accepted despite edit failure", outcome)
		}
		stub.mu.Lock()
		patches := slices.Clone(stub.patches)
		stub.mu.Unlock()
		if len(patches) != 2 {
			t.Fatalf("patches = %d, want failed edit + component clear fallback", len(patches))
		}
		if _, hasContent := patches[0].body["content"]; !hasContent {
			t.Errorf("first patch lacked content: %v", patches[0].body)
		}
		if _, hasContent := patches[1].body["content"]; hasContent {
			t.Errorf("fallback patch should only clear components: %v", patches[1].body)
		}
		if components, ok := patches[1].body["components"].([]any); !ok || len(components) != 0 {
			t.Errorf("fallback components = %v, want empty slice", patches[1].body["components"])
		}
	})
}

func TestCardTooLongFailsClosed(t *testing.T) {
	stub := newApprovalStub(t)
	adapter := newLocalizedCardAdapter(t, stub, "en", 7*24*time.Hour, nil)
	def := adapter.cardPersona("team")

	basePrompt := func() *ApprovalPrompt {
		p := testPrompt("dm:777", time.Now().Add(time.Minute))
		p.ToolName = "write_file"
		p.ToolVersion = "v1"
		p.Arguments = ""
		return p
	}
	baseContent := renderApprovalCard(basePrompt(), &def)
	baseOverhead := utf8.RuneCountInString(baseContent) - len("{}")
	longestOutcome := approval.LongestOutcomeLine(&approval.OutcomeLineInput{
		Persona: &def,
		Clean:   neutralizeMentions,
	}, false, utf8.RuneCountInString)

	t.Run("owner card over limit with outcome line posts nothing and returns approval_unavailable", func(t *testing.T) {
		prompt := basePrompt()
		prompt.Arguments = strings.Repeat("a", discordMessageLimit-baseOverhead-longestOutcome)
		if utf8.RuneCountInString(renderApprovalCard(prompt, &def)) > discordMessageLimit {
			t.Fatal("test fixture card exceeded limit even without outcome line")
		}
		accepted, err := adapter.Decide(t.Context(), prompt)
		if accepted || !fault.HasCode(err, ErrorCodeApprovalUnavailable) || !errors.Is(err, approval.ErrUnavailable) {
			t.Fatalf("Decide() = %v, %v; want approval_unavailable", accepted, err)
		}
		if posts, _, _ := stub.counts(); posts != 0 || adapter.approvals.Len() != 0 {
			t.Fatalf("posts=%d pending=%d, want 0", posts, adapter.approvals.Len())
		}
	})

	t.Run("owner card exactly at limit with outcome line posts", func(t *testing.T) {
		prompt := basePrompt()
		prompt.Arguments = strings.Repeat("a", discordMessageLimit-baseOverhead-longestOutcome-1)
		result := decideAsync(t.Context(), adapter, prompt)
		approve, _ := cardCustomIDs(t, stub.waitPost(t, 5*time.Second))
		adapter.resolveComponentClick(t.Context(), clickPayload("ix-fit", "777", "", "333", approve))
		if outcome := awaitDecide(t, result, 5*time.Second); outcome.err != nil || !outcome.accepted {
			t.Fatalf("Decide() = %+v, want accepted", outcome)
		}
	})

	t.Run("owner-DM card over limit returns approval_unavailable", func(t *testing.T) {
		dmStub := newApprovalStub(t)
		dmAdapter := newLocalizedCardAdapter(t, dmStub, "en", 7*24*time.Hour, nil)
		prompt := httpPrompt(time.Now().Add(time.Minute))
		prompt.Arguments = strings.Repeat("b", discordMessageLimit)
		accepted, err := dmAdapter.DecideForOwner(t.Context(), prompt, httpRequester(principal.ClassTrusted))
		if accepted || !fault.HasCode(err, ErrorCodeApprovalUnavailable) || !errors.Is(err, approval.ErrUnavailable) {
			t.Fatalf("DecideForOwner() = %v, %v; want approval_unavailable", accepted, err)
		}
		if posts, _, _ := dmStub.counts(); posts != 0 || dmAdapter.approvals.Len() != 0 {
			t.Fatalf("posts=%d pending=%d, want 0", posts, dmAdapter.approvals.Len())
		}
	})
}

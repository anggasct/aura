package telegram

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf16"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

func localizedPersona(t *testing.T, lang, name string) auraagent.Definition {
	t.Helper()
	registry, err := auraagent.Build([]auraagent.Persona{{
		ID:       testPersona,
		Name:     name,
		Audience: auraagent.AudienceTrusted,
	}}, []string{"read_file", "write_file", "list_dir", "exec", "web_fetch", "web_search", "aura_operation_search"}, nil, []string{"primary"}, lang)
	if err != nil {
		t.Fatalf("agent.Build(%q): %v", lang, err)
	}
	def, ok := registry.Lookup(testPersona)
	if !ok {
		t.Fatalf("persona %q missing", testPersona)
	}
	return def
}

func utf16Slice(text string, offset, length int) string {
	encoded := utf16.Encode([]rune(text))
	if offset < 0 || length < 0 || offset+length > len(encoded) {
		return ""
	}
	return string(utf16.Decode(encoded[offset : offset+length]))
}

func TestTelegramCardUsesAnExpandableBlock(t *testing.T) {
	def := localizedPersona(t, "id", "Aura 🤖✨")
	rig := newApprovalRig(t, accessOpts{definition: def})
	synctest.Test(t, func(t *testing.T) {
		rig.adapter.rememberTarget("session-emoji", ownerTarget(privateDest(ownerUserID)))
		prompt := approvalPrompt("session-emoji", principal.OwnerID)
		prompt.ToolName = "exec"
		prompt.ToolVersion = "v1"
		prompt.Arguments = `{"command":"echo 🚀"}`
		result := rig.decide(t.Context(), prompt)
		card := rig.mustCard(t, 1)

		if card.parseMode != "" {
			t.Errorf("parse_mode = %q, want empty", card.parseMode)
		}
		wantHeader := "Aura 🤖✨ mau menjalankan perintah:"
		wantDetails := "exec@v1\n" + prompt.Arguments
		if card.text != wantHeader+"\n"+wantDetails {
			t.Errorf("card text =\n%q\nwant:\n%q", card.text, wantHeader+"\n"+wantDetails)
		}
		if len(card.entities) != 1 {
			t.Fatalf("entities = %+v, want 1 expandable_blockquote", card.entities)
		}
		entity := card.entities[0]
		if entity.Type != entityExpandableBlockquote {
			t.Errorf("entity type = %q, want %q", entity.Type, entityExpandableBlockquote)
		}
		if got := utf16Slice(card.text, entity.Offset, entity.Length); got != wantDetails {
			t.Errorf("entity covers %q (offset=%d length=%d), want %q", got, entity.Offset, entity.Length, wantDetails)
		}
		if wantOffset := utf16Len(wantHeader + "\n"); entity.Offset != wantOffset {
			t.Errorf("entity offset = %d, want %d", entity.Offset, wantOffset)
		}
		if wantLen := utf16Len(wantDetails); entity.Length != wantLen {
			t.Errorf("entity length = %d, want %d", entity.Length, wantLen)
		}

		rig.press(t, "cb-1", ownerUserID, card, card.data(t, "Izinkan"))
		if outcome, done := settled(result); !done || !outcome.accepted || outcome.err != nil {
			t.Fatalf("decision = %+v (settled %v), want accepted", outcome, done)
		}
	})
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
			def := localizedPersona(t, tc.lang, "Aura")
			rig := newApprovalRig(t, accessOpts{
				definition: def,
				settings:   access.Settings{StandingGrantTTL: tc.ttl},
			})
			synctest.Test(t, func(t *testing.T) {
				result := rig.decideForOwner(t.Context(), approvalPrompt("http-session", httpPrincipal), httpRequester(principal.ClassTrusted))
				card := rig.mustCard(t, 1)
				var labels []string
				for _, button := range card.buttons {
					labels = append(labels, button.text)
				}
				if !slices.Equal(labels, tc.wantLabels) {
					t.Fatalf("button labels = %v, want %v", labels, tc.wantLabels)
				}
				tokenA, actA, _, okA := approval.ParseCallbackData(card.buttons[0].data)
				tokenS, actS, _, okS := approval.ParseCallbackData(card.buttons[1].data)
				tokenR, actR, _, okR := approval.ParseCallbackData(card.buttons[2].data)
				if !okA || !okS || !okR || actA != approval.ActionApprove || actS != approval.ActionStanding || actR != approval.ActionReject {
					t.Fatalf("callback data invalid: %+v", card.buttons)
				}
				if tokenA != tokenS || tokenA != tokenR {
					t.Fatalf("callback tokens differ across buttons: %q %q %q", tokenA, tokenS, tokenR)
				}
				rig.press(t, "cb-owner", ownerUserID, card, card.buttons[0].data)
				if outcome, done := settled(result); !done || !outcome.accepted || outcome.err != nil {
					t.Fatalf("decision = %+v (settled %v), want accepted", outcome, done)
				}
			})
		})
	}
}

func TestCardIsEditedWithTheOutcome(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
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
		{"expiry in id without timezone", "id", nil, "expiry", "⌛ Kedaluwarsa · 14:06 UTC"},
		{"cancel in owner timezone", "en", jakarta, "cancel", "⏹ Cancelled · 21:05"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := localizedPersona(t, tc.lang, "Aura")
			rig := newApprovalRig(t, accessOpts{
				definition:    def,
				ownerLocation: tc.loc,
			})
			synctest.Test(t, func(t *testing.T) {
				time.Sleep(14*time.Hour + 5*time.Minute)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				prompt := approvalPrompt("http-session", httpPrincipal)
				prompt.ToolName = "exec"
				prompt.ToolVersion = "v1"
				prompt.Arguments = `{"command":"pwd"}`
				prompt.ExpiresAt = time.Now().Add(time.Minute)
				result := rig.decideForOwner(ctx, prompt, httpRequester(principal.ClassTrusted))
				card := rig.mustCard(t, 1)

				switch tc.action {
				case "approve":
					rig.press(t, "cb-1", ownerUserID, card, card.buttons[0].data)
				case "standing":
					rig.press(t, "cb-1", ownerUserID, card, card.buttons[1].data)
				case "reject":
					rig.press(t, "cb-1", ownerUserID, card, card.buttons[2].data)
				case "expiry":
					time.Sleep(time.Minute + time.Second)
					synctest.Wait()
				case "cancel":
					cancel()
					synctest.Wait()
				}
				if _, done := settled(result); !done {
					t.Fatal("decision did not settle")
				}

				edits := rig.h.api.bodies("editMessageText")
				if len(edits) != 1 {
					t.Fatalf("editMessageText calls = %d, want 1", len(edits))
				}
				gotText, _ := edits[0]["text"].(string)
				wantText := card.text + "\n" + tc.wantLine
				if gotText != wantText {
					t.Errorf("edited text =\n%q\nwant:\n%q", gotText, wantText)
				}
				if mode, _ := edits[0]["parse_mode"].(string); mode != "" {
					t.Errorf("edit parse_mode = %q, want empty", mode)
				}
				previews, _ := edits[0]["link_preview_options"].(map[string]any)
				if previews["is_disabled"] != true {
					t.Errorf("edit link_preview_options = %v, want is_disabled true", edits[0]["link_preview_options"])
				}
				markup, _ := edits[0]["reply_markup"].(map[string]any)
				rows, present := markup["inline_keyboard"].([]any)
				if !present || len(rows) != 0 {
					t.Errorf("edit reply_markup = %v, want empty inline_keyboard", markup)
				}
				rawEntities, _ := edits[0]["entities"].([]any)
				if len(rawEntities) != 1 {
					t.Fatalf("edit entities = %v, want 1", rawEntities)
				}
				ent, _ := rawEntities[0].(map[string]any)
				entType, _ := ent["type"].(string)
				entOffset, _ := ent["offset"].(float64)
				entLen, _ := ent["length"].(float64)
				if entType != entityExpandableBlockquote || int(entOffset) != card.entities[0].Offset || int(entLen) != card.entities[0].Length {
					t.Errorf("edit entity = %v, want %+v", ent, card.entities[0])
				}
				if gotDetails := utf16Slice(gotText, int(entOffset), int(entLen)); gotDetails != "exec@v1\n"+prompt.Arguments {
					t.Errorf("edited entity details = %q, want unchanged details", gotDetails)
				}
				if got := len(rig.h.api.bodies("editMessageReplyMarkup")); got != 0 {
					t.Errorf("editMessageReplyMarkup calls = %d, want 0 when edit succeeds", got)
				}
			})
		})
	}

	t.Run("failing edit falls back to keyboard removal", func(t *testing.T) {
		rig := newApprovalRig(t, accessOpts{definition: routedDefinition()})
		synctest.Test(t, func(t *testing.T) {
			rig.adapter.rememberTarget("session-fallback", ownerTarget(privateDest(ownerUserID)))
			rig.h.api.failNextOn("editMessageText", apiFailure(http.StatusInternalServerError, 0))
			result := rig.decide(t.Context(), approvalPrompt("session-fallback", principal.OwnerID))
			card := rig.mustCard(t, 1)
			rig.press(t, "cb-1", ownerUserID, card, card.data(t, "Approve"))
			outcome, done := settled(result)
			if !done || !outcome.accepted || outcome.err != nil {
				t.Fatalf("decision = %+v (settled %v), want accepted despite edit failure", outcome, done)
			}
			if got := len(rig.h.api.bodies("editMessageText")); got != 1 {
				t.Errorf("editMessageText calls = %d, want 1", got)
			}
			removals := rig.h.api.bodies("editMessageReplyMarkup")
			if len(removals) != 1 {
				t.Fatalf("fallback editMessageReplyMarkup calls = %d, want 1", len(removals))
			}
		})
	})
}

func TestCardTooLongFailsClosed(t *testing.T) {
	def := localizedPersona(t, "en", "Aura")
	basePrompt := func() *approval.Prompt {
		p := approvalPrompt("session-limit", principal.OwnerID)
		p.ToolName = "exec"
		p.ToolVersion = "v1"
		p.Arguments = ""
		return p
	}
	baseCard := approval.RenderCard(basePrompt(), approval.CardOptions{Persona: &def})
	baseOverhead := utf16Len(baseCard.Header) + 1 + utf16Len("exec@v1\n")
	longestOutcome := approval.LongestOutcomeLine(&approval.OutcomeLineInput{Persona: &def}, false, utf16Len)

	t.Run("owner card over limit with outcome line posts nothing and returns approval_unavailable", func(t *testing.T) {
		rig := newApprovalRig(t, accessOpts{definition: def})
		synctest.Test(t, func(t *testing.T) {
			rig.adapter.rememberTarget("session-limit", ownerTarget(privateDest(ownerUserID)))
			prompt := basePrompt()
			prompt.Arguments = strings.Repeat("a", messageLimit-baseOverhead-longestOutcome)
			if utf16Len(approval.RenderCard(prompt, approval.CardOptions{Persona: &def}).Text) > messageLimit {
				t.Fatal("test fixture card exceeded limit even without outcome line")
			}
			accepted, err := rig.adapter.Decide(t.Context(), prompt)
			if accepted || !fault.HasCode(err, ErrorCodeApprovalUnavailable) || !errors.Is(err, approval.ErrUnavailable) {
				t.Fatalf("Decide() = %v, %v; want approval_unavailable", accepted, err)
			}
			if len(rig.h.api.sends()) != 0 {
				t.Fatalf("messages sent = %d, want 0", len(rig.h.api.sends()))
			}
			if rig.adapter.approvals.Len() != 0 {
				t.Fatalf("pending tokens = %d, want 0", rig.adapter.approvals.Len())
			}
		})
	})

	t.Run("owner card exactly at limit with outcome line posts", func(t *testing.T) {
		rig := newApprovalRig(t, accessOpts{definition: def})
		synctest.Test(t, func(t *testing.T) {
			rig.adapter.rememberTarget("session-limit", ownerTarget(privateDest(ownerUserID)))
			prompt := basePrompt()
			prompt.Arguments = strings.Repeat("a", messageLimit-baseOverhead-longestOutcome-1)
			result := rig.decide(t.Context(), prompt)
			card := rig.mustCard(t, 1)
			rig.press(t, "cb-fit", ownerUserID, card, card.data(t, "Approve"))
			if outcome, done := settled(result); !done || !outcome.accepted || outcome.err != nil {
				t.Fatalf("decision = %+v (settled %v), want accepted", outcome, done)
			}
		})
	})

	t.Run("owner-DM card over limit with standing outcome line returns approval_unavailable", func(t *testing.T) {
		rig := newApprovalRig(t, accessOpts{definition: def})
		synctest.Test(t, func(t *testing.T) {
			prompt := approvalPrompt("http-limit", httpPrincipal)
			prompt.ToolName = "exec"
			prompt.ToolVersion = "v1"
			prompt.Arguments = strings.Repeat("b", messageLimit)
			accepted, err := rig.adapter.DecideForOwner(t.Context(), prompt, httpRequester(principal.ClassTrusted))
			if accepted || !fault.HasCode(err, ErrorCodeApprovalUnavailable) || !errors.Is(err, approval.ErrUnavailable) {
				t.Fatalf("DecideForOwner() = %v, %v; want approval_unavailable", accepted, err)
			}
			if len(rig.h.api.sends()) != 0 || rig.adapter.approvals.Len() != 0 {
				t.Fatalf("sends=%d pending=%d, want 0", len(rig.h.api.sends()), rig.adapter.approvals.Len())
			}
		})
	})
}

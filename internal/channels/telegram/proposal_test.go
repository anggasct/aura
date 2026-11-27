package telegram

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf16"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	policy "github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

const proposalHead = "workflow proposal prop_1"

type proposalRecorder struct {
	mu    sync.Mutex
	got   []bool
	reply string
	err   error
}

func (r *proposalRecorder) decide(_ context.Context, approved bool) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, approved)
	return r.reply, r.err
}

func (r *proposalRecorder) decisions() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

type failingTransport struct {
	err error
}

func (t failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

func unitsOf(text string) int {
	return len(utf16.Encode([]rune(text)))
}

func planOfUnits(units int) string {
	plan := "Plan: "
	remaining := units - unitsOf(plan)
	return plan + strings.Repeat("\U0001F600", remaining/2) + strings.Repeat("a", remaining%2)
}

func entityFields(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := body["entities"].([]any)
	entities := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		entity, _ := item.(map[string]any)
		entities = append(entities, entity)
	}
	return entities
}

func mustOffer(t *testing.T, rig *approvalRig, card approval.ProposalCard, recorded *proposalRecorder) postedCard {
	t.Helper()
	if err := rig.adapter.OfferProposal(t.Context(), card, recorded.decide); err != nil {
		t.Fatalf("OfferProposal() = %v", err)
	}
	return rig.mustCard(t, 1)
}

func TestProposalCardIsSentToTheOwnerDMWithApproveAndReject(t *testing.T) {
	plan := "Plan: ship https://example.com/x?a=1 to @release_bot with #urgent \U0001F680 now\n1. approve - owner approval"
	cases := []struct {
		name     string
		tainted  bool
		wantHead string
	}{
		{"trusted input", false, proposalHead + "\n"},
		{"untrusted input", true, proposalHead + "\n" + policy.UntrustedInputNotice + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApprovalRig(t, accessOpts{})
			synctest.Test(t, func(t *testing.T) {
				card := mustOffer(t, rig, approval.ProposalCard{ID: "prop_1", Plan: plan, Tainted: tc.tainted}, &proposalRecorder{})
				body := rig.h.api.bodies("sendMessage")[0]
				if card.chatID != ownerUserID || card.threadID != 0 {
					t.Fatalf("card posted to %d/%d, want the owner's private chat", card.chatID, card.threadID)
				}
				if _, present := body["parse_mode"]; present {
					t.Errorf("card carries parse_mode %v", body["parse_mode"])
				}
				if previews, _ := body["link_preview_options"].(map[string]any); previews["is_disabled"] != true {
					t.Errorf("link previews are not disabled: %v", body["link_preview_options"])
				}
				if card.text != tc.wantHead+plan {
					t.Fatalf("card text = %q, want the header block then the plan", card.text)
				}
				entities := entityFields(t, body)
				if len(entities) != 1 || entities[0]["type"] != "pre" {
					t.Fatalf("entities = %v, want exactly one pre", entities)
				}
				rawOffset, _ := entities[0]["offset"].(float64)
				rawLength, _ := entities[0]["length"].(float64)
				offset, length := int(rawOffset), int(rawLength)
				units := utf16.Encode([]rune(card.text))
				if covered := string(utf16.Decode(units[offset : offset+length])); covered != plan || offset+length != len(units) {
					t.Errorf("pre entity covers %q at %d+%d of %d units, want exactly the plan", covered, offset, length, len(units))
				}
				if len(card.buttons) != 2 || card.buttons[0].text != "Approve" || card.buttons[1].text != "Reject" {
					t.Fatalf("card buttons = %+v, want approve and reject only", card.buttons)
				}
				for _, button := range card.buttons {
					if _, _, _, ok := approval.ParseCallbackData(button.data); !ok || len(button.data) > 64 {
						t.Errorf("callback data %q is unparsable or over 64 bytes", button.data)
					}
				}
			})
		})
	}
}

func TestProposalCardTooLongPostsTheNoticeWithoutAToken(t *testing.T) {
	headUnits := unitsOf(proposalHead + "\n")
	cases := []struct {
		name       string
		tainted    bool
		units      int
		wantNotice string
	}{
		{name: "exactly the limit gets the card", units: messageLimit - headUnits},
		{
			name:       "one unit over gets the notice",
			units:      messageLimit - headUnits + 1,
			wantNotice: proposalHead + " is too long for a card. Review it with aura workflow proposal show prop_1 and approve it there.",
		},
		{
			name:       "untrusted input keeps its line in the notice",
			tainted:    true,
			units:      messageLimit,
			wantNotice: proposalHead + " is too long for a card. Review it with aura workflow proposal show prop_1 and approve it there.\n" + policy.UntrustedInputNotice,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApprovalRig(t, accessOpts{})
			synctest.Test(t, func(t *testing.T) {
				plan := planOfUnits(tc.units)
				if len([]rune(plan)) >= messageLimit {
					t.Fatalf("plan has %d runes, the test needs astral runes to tell units from runes", len([]rune(plan)))
				}
				recorded := &proposalRecorder{}
				if err := rig.adapter.OfferProposal(t.Context(), approval.ProposalCard{ID: "prop_1", Plan: plan, Tainted: tc.tainted}, recorded.decide); err != nil {
					t.Fatalf("OfferProposal() = %v", err)
				}
				synctest.Wait()
				sends := rig.h.api.bodies("sendMessage")
				if len(sends) != 1 {
					t.Fatalf("sends = %d, want 1", len(sends))
				}
				if tc.wantNotice == "" {
					if sends[0]["reply_markup"] == nil || rig.adapter.approvals.Len() != 1 {
						t.Fatalf("a card at the limit has no keyboard or token: %v", sends[0])
					}
					return
				}
				text, _ := sends[0]["text"].(string)
				if text != tc.wantNotice || strings.Contains(text, "`") {
					t.Errorf("notice = %q, want %q without backticks", text, tc.wantNotice)
				}
				if sends[0]["reply_markup"] != nil || sends[0]["entities"] != nil {
					t.Errorf("notice carries a keyboard or entities: %v", sends[0])
				}
				if got := rig.adapter.approvals.Len(); got != 0 {
					t.Errorf("pending tokens = %d, want none", got)
				}
			})
		})
	}
}

func TestProposalCardTooLongNoticeFailureIsUnavailable(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	rig.h.api.failNextOn("sendMessage", apiFailure(http.StatusBadRequest, 0))
	synctest.Test(t, func(t *testing.T) {
		err := rig.adapter.OfferProposal(t.Context(), approval.ProposalCard{ID: "prop_1", Plan: planOfUnits(messageLimit + 10)}, (&proposalRecorder{}).decide)
		if !fault.HasCode(err, ErrorCodeApprovalUnavailable) {
			t.Fatalf("OfferProposal() = %v, want approval_unavailable", err)
		}
	})
}

func TestProposalCardNeedsATelegramOwner(t *testing.T) {
	cases := []struct {
		name   string
		owners []string
		token  string
	}{
		{"no telegram owner identity", []string{"discord:123456789012345678"}, testToken},
		{"no bot token", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := accessOpts{}
			if tc.owners != nil {
				owners, err := principal.NewResolver(tc.owners)
				if err != nil {
					t.Fatalf("NewResolver: %v", err)
				}
				opts.owners = owners
			}
			rig := newApprovalRig(t, opts)
			t.Setenv(testTokenEnv, tc.token)
			synctest.Test(t, func(t *testing.T) {
				err := rig.adapter.OfferProposal(t.Context(), approval.ProposalCard{ID: "prop_1", Plan: "Plan: x"}, (&proposalRecorder{}).decide)
				if !fault.HasCode(err, ErrorCodeApprovalUnavailable) {
					t.Fatalf("OfferProposal() = %v, want approval_unavailable", err)
				}
				if got := rig.h.api.count("sendMessage"); got != 0 {
					t.Errorf("sends = %d, want none", got)
				}
			})
		})
	}
}

func TestProposalCardDefinitiveSendFailuresAreUnavailableAndDropTheToken(t *testing.T) {
	cases := []struct {
		name  string
		setup func(rig *approvalRig)
	}{
		{"owner never started the bot", func(rig *approvalRig) {
			rig.h.api.failNextOn("sendMessage", describedFailure(http.StatusForbidden, "Forbidden: bot can't initiate conversation with a user", 0))
		}},
		{"bot api failure", func(rig *approvalRig) {
			rig.h.api.failNextOn("sendMessage", apiFailure(http.StatusInternalServerError, 0))
		}},
		{"dial failure", func(rig *approvalRig) {
			rig.adapter.bot.http = &http.Client{Transport: failingTransport{err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApprovalRig(t, accessOpts{})
			tc.setup(rig)
			synctest.Test(t, func(t *testing.T) {
				err := rig.adapter.OfferProposal(t.Context(), approval.ProposalCard{ID: "prop_1", Plan: "Plan: x"}, (&proposalRecorder{}).decide)
				if !fault.HasCode(err, ErrorCodeApprovalUnavailable) || errors.Is(err, runtimechannelhost.ErrDeliveryAmbiguous) {
					t.Fatalf("OfferProposal() = %v, want a definitive approval_unavailable", err)
				}
				if rig.adapter.approvals.Len() != 0 {
					t.Error("pending token kept after the card failed")
				}
			})
		})
	}
}

func TestProposalCardWithoutAMessageIDDropsTheTokenAndIsAmbiguous(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	rig.h.api.failNextOn("sendMessage", func(w http.ResponseWriter) bool {
		writeOK(w, map[string]any{"message_id": 0})
		return true
	})
	synctest.Test(t, func(t *testing.T) {
		recorded := &proposalRecorder{}
		err := rig.adapter.OfferProposal(t.Context(), approval.ProposalCard{ID: "prop_1", Plan: "Plan: x"}, recorded.decide)
		if !errors.Is(err, runtimechannelhost.ErrDeliveryAmbiguous) {
			t.Fatalf("OfferProposal() = %v, want an ambiguous delivery", err)
		}
		if rig.adapter.approvals.Len() != 0 {
			t.Error("pending token kept after an ambiguous send")
		}
	})
}

func TestProposalCardTransportTimeoutIsAmbiguous(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	rig.adapter.bot.http = &http.Client{Transport: failingTransport{err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}}}
	synctest.Test(t, func(t *testing.T) {
		err := rig.adapter.OfferProposal(t.Context(), approval.ProposalCard{ID: "prop_1", Plan: "Plan: x"}, (&proposalRecorder{}).decide)
		if !errors.Is(err, runtimechannelhost.ErrDeliveryAmbiguous) {
			t.Fatalf("OfferProposal() = %v, want an ambiguous delivery", err)
		}
		if rig.adapter.approvals.Len() != 0 {
			t.Error("pending token kept after an ambiguous send")
		}
	})
}

func TestProposalCardReportsAnUndecidableProposal(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	synctest.Test(t, func(t *testing.T) {
		recorded := &proposalRecorder{err: errors.New("workflow_proposal_digest_mismatch: changed")}
		card := mustOffer(t, rig, approval.ProposalCard{ID: "prop_1", Plan: "Plan: x"}, recorded)
		rig.press(t, "cb-1", ownerUserID, card, card.data(t, "Approve"))
		want := []string{"proposal prop_1 could not be decided: workflow_proposal_digest_mismatch: changed"}
		if got := noticesTo(rig, privateDest(ownerUserID)); !slices.Equal(got, want) {
			t.Fatalf("dm notices = %q, want %q", got, want)
		}
	})
}

func TestProposalCardIsDecidedOnlyByTheOwnerInTheDM(t *testing.T) {
	approved := []struct {
		name   string
		label  string
		accept bool
		reply  string
	}{
		{"owner approves", "Approve", true, "approved prop_1: run run_9"},
		{"owner rejects", "Reject", false, "rejected prop_1"},
	}
	for _, tc := range approved {
		t.Run(tc.name, func(t *testing.T) {
			rig := newApprovalRig(t, accessOpts{})
			synctest.Test(t, func(t *testing.T) {
				recorded := &proposalRecorder{reply: tc.reply}
				card := mustOffer(t, rig, approval.ProposalCard{ID: "prop_1", Plan: "Plan: x"}, recorded)
				rig.press(t, "cb-1", ownerUserID, card, card.data(t, tc.label))
				if got := recorded.decisions(); !slices.Equal(got, []bool{tc.accept}) {
					t.Fatalf("decisions = %v, want [%v]", got, tc.accept)
				}
				assertKeyboardRemoved(t, rig, card)
				if got := noticesTo(rig, privateDest(ownerUserID)); !slices.Equal(got, []string{tc.reply}) {
					t.Errorf("dm notices = %q, want the decision result", got)
				}
				if got := rig.dropped(); len(got) != 0 {
					t.Errorf("drop records = %v, want none", got)
				}
			})
		})
	}

	topic := destination{chatID: groupChatID, threadID: forumTopic}
	rejected := []struct {
		name      string
		press     func(t *testing.T, rig *approvalRig, card postedCard)
		wantDrops int
	}{
		{"another user", func(t *testing.T, rig *approvalRig, card postedCard) {
			rig.press(t, "cb-bad", strangerID, card, card.data(t, "Approve"))
		}, 1},
		{"the owner in another chat", func(t *testing.T, rig *approvalRig, card postedCard) {
			rig.pressAt(t, "cb-bad", ownerUserID, destination{chatID: groupChatID}, card.messageID, card.data(t, "Approve"))
		}, 1},
		{"the owner in a forum topic", func(t *testing.T, rig *approvalRig, card postedCard) {
			rig.pressAt(t, "cb-bad", ownerUserID, topic, card.messageID, card.data(t, "Approve"))
		}, 1},
		{"a tampered mac", func(t *testing.T, rig *approvalRig, card postedCard) {
			rig.press(t, "cb-bad", ownerUserID, card, flipLast(card.data(t, "Approve")))
		}, 1},
		{"a standing action with a valid mac", func(t *testing.T, rig *approvalRig, card postedCard) {
			tokenID, _, _, _ := approval.ParseCallbackData(card.data(t, "Approve"))
			standing := approval.Binding{Actor: strconv.Itoa(ownerUserID), Scope: scopeOf(privateDest(ownerUserID)), Standing: true}
			data := rig.adapter.approvals.Callbacks(tokenID, &standing, time.Now().Add(rig.adapter.proposalTTL)).Standing
			rig.press(t, "cb-bad", ownerUserID, card, data)
		}, 1},
		{"a forged standing action", func(t *testing.T, rig *approvalRig, card postedCard) {
			rig.press(t, "cb-bad", ownerUserID, card, strings.Replace(card.data(t, "Approve"), ":a:", ":s:", 1))
		}, 1},
		{"a replayed callback id", func(t *testing.T, rig *approvalRig, card postedCard) {
			rig.press(t, "cb-bad", strangerID, card, card.data(t, "Approve"))
			rig.press(t, "cb-bad", ownerUserID, card, card.data(t, "Approve"))
		}, 2},
	}
	for _, tc := range rejected {
		t.Run(tc.name+" decides nothing", func(t *testing.T) {
			rig := newApprovalRig(t, accessOpts{})
			synctest.Test(t, func(t *testing.T) {
				recorded := &proposalRecorder{reply: "approved prop_1: run run_9"}
				card := mustOffer(t, rig, approval.ProposalCard{ID: "prop_1", Plan: "Plan: x"}, recorded)
				tc.press(t, rig, card)
				if got := recorded.decisions(); len(got) != 0 {
					t.Fatalf("decisions = %v, want none", got)
				}
				want := slices.Repeat([]string{string(dropCallbackRejected)}, tc.wantDrops)
				if got := rig.dropped(); !slices.Equal(got, want) {
					t.Fatalf("drop records = %v, want %v", got, want)
				}
				if rig.adapter.approvals.Len() != 1 {
					t.Error("a rejected callback consumed the pending token")
				}
				rig.press(t, "cb-good", ownerUserID, card, card.data(t, "Approve"))
				if got := recorded.decisions(); !slices.Equal(got, []bool{true}) {
					t.Errorf("decisions after the owner's own press = %v, want [true]", got)
				}
			})
		})
	}

	t.Run("a second press after a decision decides nothing", func(t *testing.T) {
		rig := newApprovalRig(t, accessOpts{})
		synctest.Test(t, func(t *testing.T) {
			recorded := &proposalRecorder{reply: "approved prop_1: run run_9"}
			card := mustOffer(t, rig, approval.ProposalCard{ID: "prop_1", Plan: "Plan: x"}, recorded)
			rig.press(t, "cb-1", ownerUserID, card, card.data(t, "Approve"))
			rig.press(t, "cb-2", ownerUserID, card, card.data(t, "Reject"))
			if got := recorded.decisions(); !slices.Equal(got, []bool{true}) {
				t.Fatalf("decisions = %v, want [true]", got)
			}
			if got := rig.dropped(); !slices.Equal(got, []string{string(dropCallbackRejected)}) {
				t.Errorf("drop records = %v, want one callback_rejected", got)
			}
		})
	})

	t.Run("a press after expiry decides nothing", func(t *testing.T) {
		rig := newApprovalRig(t, accessOpts{})
		rig.adapter.proposalTTL = time.Minute
		synctest.Test(t, func(t *testing.T) {
			recorded := &proposalRecorder{}
			card := mustOffer(t, rig, approval.ProposalCard{ID: "prop_1", Plan: "Plan: x"}, recorded)
			time.Sleep(2 * time.Minute)
			synctest.Wait()
			assertKeyboardRemoved(t, rig, card)
			rig.press(t, "cb-late", ownerUserID, card, card.data(t, "Approve"))
			if got := recorded.decisions(); len(got) != 0 {
				t.Fatalf("decisions = %v, want none", got)
			}
			if got := rig.dropped(); !slices.Equal(got, []string{string(dropCallbackRejected)}) {
				t.Errorf("drop records = %v, want one callback_rejected", got)
			}
		})
	})
}

func TestProposalCardWaitEndsWhenTheContextIsCancelled(t *testing.T) {
	rig := newApprovalRig(t, accessOpts{})
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		recorded := &proposalRecorder{}
		if err := rig.adapter.OfferProposal(ctx, approval.ProposalCard{ID: "prop_1", Plan: "Plan: x"}, recorded.decide); err != nil {
			t.Fatalf("OfferProposal() = %v", err)
		}
		card := rig.mustCard(t, 1)
		cancel()
		synctest.Wait()
		assertKeyboardRemoved(t, rig, card)
		if rig.adapter.approvals.Len() != 0 {
			t.Error("pending token kept after the wait ended")
		}
		if got := recorded.decisions(); len(got) != 0 {
			t.Errorf("decisions = %v, want none", got)
		}
	})
}

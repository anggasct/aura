package workflow

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"

	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

type scriptedOfferer struct {
	name   string
	serves bool
	err    error
	tried  *[]string
}

func (o *scriptedOfferer) OfferProposal(_ context.Context, _ ProposalCard, _ func(ctx context.Context, approved bool) (string, error)) error {
	*o.tried = append(*o.tried, o.name)
	return o.err
}

func (o *scriptedOfferer) ServesSession(string) bool {
	return o.serves
}

type recordedWarnings struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *recordedWarnings) Enabled(context.Context, slog.Level) bool { return true }

func (r *recordedWarnings) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
	return nil
}

func (r *recordedWarnings) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *recordedWarnings) WithGroup(string) slog.Handler { return r }

func attrOf(record slog.Record, key string) (slog.Value, bool) {
	var found slog.Value
	var ok bool
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			found, ok = attr.Value, true
			return false
		}
		return true
	})
	return found, ok
}

func TestProposalCardTriesTheServingChannelThenTheOthers(t *testing.T) {
	discordDown := errors.New("discord card unavailable")
	telegramDown := errors.New("telegram card unavailable")
	ambiguous := fmt.Errorf("card send outcome is unknown: %w", runtimechannelhost.ErrDeliveryAmbiguous)
	cases := []struct {
		name          string
		offerers      []scriptedOfferer
		wantTried     []string
		wantAmbiguous bool
		wantWarning   bool
		wantJoined    []string
	}{
		{
			name:      "the serving offerer delivers and is the only one tried",
			offerers:  []scriptedOfferer{{name: "discord"}, {name: "telegram", serves: true}},
			wantTried: []string{"telegram"},
		},
		{
			name:      "a serving offerer that is unavailable falls through in channel order",
			offerers:  []scriptedOfferer{{name: "discord"}, {name: "telegram", serves: true, err: telegramDown}, {name: "email"}},
			wantTried: []string{"telegram", "discord"},
		},
		{
			name:      "without a serving offerer the first success stops the search",
			offerers:  []scriptedOfferer{{name: "discord", err: discordDown}, {name: "telegram"}, {name: "email"}},
			wantTried: []string{"discord", "telegram"},
		},
		{
			name:          "an ambiguous send stops the search before the later offerers",
			offerers:      []scriptedOfferer{{name: "discord", serves: true, err: ambiguous}, {name: "telegram"}},
			wantTried:     []string{"discord"},
			wantAmbiguous: true,
			wantWarning:   true,
			wantJoined:    []string{"discord"},
		},
		{
			name:        "when every offerer fails one warning carries every error",
			offerers:    []scriptedOfferer{{name: "discord", err: discordDown}, {name: "telegram", serves: true, err: telegramDown}},
			wantTried:   []string{"telegram", "discord"},
			wantWarning: true,
			wantJoined:  []string{"telegram", "discord"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tried []string
			errs := map[string]error{}
			offerers := make([]ProposalOfferer, len(tc.offerers))
			for i := range tc.offerers {
				tc.offerers[i].tried = &tried
				errs[tc.offerers[i].name] = tc.offerers[i].err
				offerers[i] = &tc.offerers[i]
			}
			warnings := &recordedWarnings{}
			controller := &Controller{cards: &proposalCards{offerers: offerers, logger: slog.New(warnings)}}
			controller.deliverProposalCard(t.Context(), &proposalOffer{sessionID: "ops/session", card: ProposalCard{ID: "prop_1", Plan: "Plan: x"}})
			if !slices.Equal(tried, tc.wantTried) {
				t.Fatalf("offerers tried = %v, want %v", tried, tc.wantTried)
			}
			if !tc.wantWarning {
				if len(warnings.records) != 0 {
					t.Fatalf("warnings = %d, want none after a delivered card", len(warnings.records))
				}
				return
			}
			if len(warnings.records) != 1 || warnings.records[0].Message != "proposal card unavailable" {
				t.Fatalf("warnings = %v, want one proposal card unavailable", warnings.records)
			}
			joined, _ := attrOf(warnings.records[0], "error")
			joinedErr, _ := joined.Any().(error)
			for _, name := range tc.wantJoined {
				if !errors.Is(joinedErr, errs[name]) {
					t.Errorf("logged error does not carry the %s failure", name)
				}
			}
			flag, _ := attrOf(warnings.records[0], "ambiguous")
			if flag.Bool() != tc.wantAmbiguous {
				t.Errorf("ambiguous = %v, want %v", flag.Bool(), tc.wantAmbiguous)
			}
		})
	}
}

func TestProposalCardQueueFullWarningCarriesTheAmbiguousAttribute(t *testing.T) {
	warnings := &recordedWarnings{}
	controller := &Controller{cards: &proposalCards{
		logger:   slog.New(warnings),
		requests: make(chan proposalOffer, 1),
	}}
	controller.cards.requests <- proposalOffer{}
	controller.offerProposal(t.Context(), "ops/session", ProposalCard{ID: "prop_1"})
	if len(warnings.records) != 1 || warnings.records[0].Message != "proposal card unavailable" {
		t.Fatalf("warnings = %v, want one proposal card unavailable", warnings.records)
	}
	flag, ok := attrOf(warnings.records[0], "ambiguous")
	if !ok || flag.Bool() {
		t.Fatalf("ambiguous = (%v, %v), want present and false", flag.Bool(), ok)
	}
}

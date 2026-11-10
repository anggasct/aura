package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/platform/principal"
)

func telegramHandoffUnavailable() error {
	return telegram.Errorf(telegram.ErrorCodeHandoffUnavailable, "owner has no telegram identity")
}

func TestOwnerHandoffRouterOrdersAndFallsThrough(t *testing.T) {
	failure := errors.New("owner dm exploded")
	cases := []struct {
		name      string
		build     func(log *askLog) []ownerHandoffNotifier
		wantErr   func(error) bool
		wantAsked []string
	}{
		{
			name: "persona serving notifier goes first",
			build: func(log *askLog) []ownerHandoffNotifier {
				return []ownerHandoffNotifier{
					&scriptedApprover{name: "discord", log: log},
					&scriptedApprover{name: "telegram", serves: "lobby", log: log},
				}
			},
			wantAsked: []string{"telegram"},
		},
		{
			name: "config order holds among notifiers that serve the persona",
			build: func(log *askLog) []ownerHandoffNotifier {
				return []ownerHandoffNotifier{
					&scriptedApprover{name: "discord", serves: "lobby", log: log},
					&scriptedApprover{name: "telegram", serves: "lobby", log: log},
				}
			},
			wantAsked: []string{"discord"},
		},
		{
			name: "config order holds when none serves the persona",
			build: func(log *askLog) []ownerHandoffNotifier {
				return []ownerHandoffNotifier{
					&scriptedApprover{name: "discord", handoffErr: discordUnavailable(), log: log},
					&scriptedApprover{name: "telegram", log: log},
				}
			},
			wantAsked: []string{"discord", "telegram"},
		},
		{
			name: "unavailable serving notifier falls through to the rest",
			build: func(log *askLog) []ownerHandoffNotifier {
				return []ownerHandoffNotifier{
					&scriptedApprover{name: "discord", log: log},
					&scriptedApprover{name: "telegram", serves: "lobby", handoffErr: telegramHandoffUnavailable(), log: log},
				}
			},
			wantAsked: []string{"telegram", "discord"},
		},
		{
			name: "a failed notice falls through to the next notifier",
			build: func(log *askLog) []ownerHandoffNotifier {
				return []ownerHandoffNotifier{
					&scriptedApprover{name: "discord", handoffErr: failure, log: log},
					&scriptedApprover{name: "telegram", log: log},
				}
			},
			wantAsked: []string{"discord", "telegram"},
		},
		{
			name: "every notifier failing reports the last failure",
			build: func(log *askLog) []ownerHandoffNotifier {
				return []ownerHandoffNotifier{
					&scriptedApprover{name: "discord", handoffErr: discordUnavailable(), log: log},
					&scriptedApprover{name: "telegram", handoffErr: failure, log: log},
				}
			},
			wantErr:   func(err error) bool { return errors.Is(err, failure) },
			wantAsked: []string{"discord", "telegram"},
		},
		{
			name:      "no notifiers fails",
			build:     func(*askLog) []ownerHandoffNotifier { return nil },
			wantErr:   func(err error) bool { return errors.Is(err, errNoOwnerChannel) },
			wantAsked: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &askLog{}
			router := &ownerHandoffRouter{notifiers: tc.build(log)}
			err := router.NotifyOwner(t.Context(), routedRequester(), "lobby/http/key/session", "needs a human")
			if (tc.wantErr == nil) != (err == nil) || (tc.wantErr != nil && !tc.wantErr(err)) {
				t.Errorf("err = %v", err)
			}
			if got := log.approvers(); !slices.Equal(got, tc.wantAsked) {
				t.Errorf("asked = %v, want %v", got, tc.wantAsked)
			}
			if len(tc.wantAsked) > 0 {
				first := log.first()
				if first.requester != *routedRequester() || first.session != "lobby/http/key/session" || first.reason != "needs a human" {
					t.Errorf("first notification = %+v", first)
				}
			}
		})
	}
}

func TestOwnerHandoffRouterStopsWhenTheTurnIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	log := &askLog{}
	router := &ownerHandoffRouter{notifiers: []ownerHandoffNotifier{
		&scriptedApprover{name: "discord", handoffErr: context.Canceled, log: log},
		&scriptedApprover{name: "telegram", log: log},
	}}

	if err := router.NotifyOwner(ctx, routedRequester(), "lobby/http/key/session", "needs a human"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
	if got := log.approvers(); !slices.Equal(got, []string{"discord"}) {
		t.Errorf("asked = %v", got)
	}
}

type heldRequesters map[string]approval.Requester

func (h heldRequesters) RequesterOf(sessionID string) (approval.Requester, bool) {
	requester, ok := h[sessionID]
	return requester, ok
}

func TestHTTPHandoffNotifierTargetsTheKeyAccount(t *testing.T) {
	const session = "lobby/http/abcdefghijkl/token"
	held := heldRequesters{session: {Principal: "http:abcdefghijkl", Class: principal.ClassPublic, Persona: "lobby"}}
	t.Run("held session", func(t *testing.T) {
		log := &askLog{}
		notifier := newHTTPHandoffNotifier(held, []approvalAdapter{&scriptedApprover{name: "telegram", log: log}})
		target, err := notifier.NotifyHandoff(t.Context(), session, "refund")
		if err != nil || target != (access.HandoffTarget{Channel: "http", Account: "abcdefghijkl"}) {
			t.Fatalf("NotifyHandoff = %+v, %v", target, err)
		}
		if got := log.first(); got.session != session || got.reason != "refund" || got.requester.Principal != "http:abcdefghijkl" {
			t.Errorf("owner notification = %+v", got)
		}
	})
	t.Run("unknown session is left to other notifiers", func(t *testing.T) {
		log := &askLog{}
		notifier := newHTTPHandoffNotifier(held, []approvalAdapter{&scriptedApprover{name: "telegram", log: log}})
		if _, err := notifier.NotifyHandoff(t.Context(), "lobby/discord:office/dm:9", "x"); !errors.Is(err, access.ErrNoHandoffTarget) {
			t.Fatalf("err = %v, want ErrNoHandoffTarget", err)
		}
		if got := log.approvers(); len(got) != 0 {
			t.Errorf("owner notified for a session the http channel does not hold: %v", got)
		}
	})
	unusable := map[string]string{"owner": principal.OwnerID, "foreign": "discord:601", "badshape": "http:short"}
	for name, requesterPrincipal := range unusable {
		t.Run("unusable requester "+name+" fails before notifying", func(t *testing.T) {
			log := &askLog{}
			requesters := heldRequesters{name: {Principal: requesterPrincipal, Persona: "lobby"}}
			notifier := newHTTPHandoffNotifier(requesters, []approvalAdapter{&scriptedApprover{name: "telegram", log: log}})
			target, err := notifier.NotifyHandoff(t.Context(), name, "x")
			if err == nil || errors.Is(err, access.ErrNoHandoffTarget) || target != (access.HandoffTarget{}) {
				t.Errorf("NotifyHandoff = %+v, %v; want a hard failure", target, err)
			}
			if got := log.approvers(); len(got) != 0 {
				t.Errorf("owner notified for an unusable requester: %v", got)
			}
		})
	}
	t.Run("unreachable owner opens nothing", func(t *testing.T) {
		notifier := newHTTPHandoffNotifier(held, []approvalAdapter{&scriptedApprover{name: "telegram", handoffErr: telegramHandoffUnavailable(), log: &askLog{}}})
		target, err := notifier.NotifyHandoff(t.Context(), session, "x")
		if err == nil || target != (access.HandoffTarget{}) {
			t.Fatalf("NotifyHandoff = %+v, %v; want a failure", target, err)
		}
	})
}

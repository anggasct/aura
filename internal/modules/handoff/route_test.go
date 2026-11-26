package handoff

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

type recordedOwnerNotice struct {
	name      string
	requester OwnerRequester
	session   string
	reason    string
}

type scriptedOwnerNotifier struct {
	name   string
	serves string
	err    error
	log    *[]recordedOwnerNotice
}

func (n *scriptedOwnerNotifier) ServesPersona(persona string) bool {
	return n.serves != "" && n.serves == persona
}

func (n *scriptedOwnerNotifier) NotifyOwnerHandoff(_ context.Context, requester *OwnerRequester, sessionID, reason string) error {
	*n.log = append(*n.log, recordedOwnerNotice{name: n.name, requester: *requester, session: sessionID, reason: reason})
	return n.err
}

type mapRequesterLookup map[string]OwnerRequester

func (m mapRequesterLookup) RequesterOf(sessionID string) (OwnerRequester, bool) {
	req, ok := m[sessionID]
	return req, ok
}

func TestOwnerRouterOrdersAndStopsOnCancel(t *testing.T) {
	failure := errors.New("owner dm failed")
	requester := &OwnerRequester{Principal: "http:abcdefghijkl", Class: principal.ClassPublic, Persona: "lobby"}

	var log []recordedOwnerNotice
	router := NewOwnerRouter([]OwnerNotifier{
		&scriptedOwnerNotifier{name: "discord", log: &log},
		&scriptedOwnerNotifier{name: "telegram", serves: "lobby", log: &log},
	})
	if err := router.NotifyOwner(t.Context(), nil, "s", "r"); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("nil requester err = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if err := router.NotifyOwner(t.Context(), requester, "s", "r"); err != nil {
		t.Fatalf("NotifyOwner: %v", err)
	}
	if len(log) != 1 || log[0].name != "telegram" {
		t.Fatalf("log = %+v, want [telegram]", log)
	}

	if err := NewOwnerRouter(nil).NotifyOwner(t.Context(), requester, "s", "r"); !errors.Is(err, ErrNoOwnerChannel) {
		t.Fatalf("empty router err = %v, want ErrNoOwnerChannel", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	log = nil
	cancelRouter := NewOwnerRouter([]OwnerNotifier{
		&scriptedOwnerNotifier{name: "discord", err: context.Canceled, log: &log},
		&scriptedOwnerNotifier{name: "telegram", err: failure, log: &log},
	})
	if err := cancelRouter.NotifyOwner(ctx, requester, "s", "r"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err = %v, want context.Canceled", err)
	}
	if names := []string{log[0].name}; !slices.Equal(names, []string{"discord"}) || len(log) != 1 {
		t.Fatalf("cancelled log = %+v, want [discord]", log)
	}
}

func TestHTTPNotifierValidationAndRouting(t *testing.T) {
	if _, err := NewHTTPNotifier(nil, NewOwnerRouter(nil)); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("nil lookup err = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if _, err := NewHTTPNotifier(mapRequesterLookup{}, nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("nil router err = %v, want %s", err, ErrorCodeInvalidArgument)
	}

	var log []recordedOwnerNotice
	notifier, err := NewHTTPNotifier(
		mapRequesterLookup{"sess-1": {Principal: "http:abcdefghijkl", Class: principal.ClassPublic, Persona: "lobby"}},
		NewOwnerRouter([]OwnerNotifier{&scriptedOwnerNotifier{name: "telegram", log: &log}}),
	)
	if err != nil {
		t.Fatalf("NewHTTPNotifier: %v", err)
	}
	target, err := notifier.NotifyHandoff(t.Context(), "sess-1", "refund")
	if err != nil || target != (Target{Channel: principal.ChannelHTTP, Account: "abcdefghijkl"}) {
		t.Fatalf("NotifyHandoff = %+v, %v", target, err)
	}
	if _, err := notifier.NotifyHandoff(t.Context(), "missing", "refund"); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("missing session err = %v, want ErrNoTarget", err)
	}
}

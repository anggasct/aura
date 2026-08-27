package cli

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"
)

type fakeTerminalSessionService struct {
	created *store.Session
}

func (f *fakeTerminalSessionService) Create(_ context.Context, sess *store.Session) error {
	f.created = sess
	return nil
}

func (f *fakeTerminalSessionService) Get(context.Context, string) (store.Session, error) {
	return store.Session{}, errors.New("not implemented")
}

func (f *fakeTerminalSessionService) ListEvents(context.Context, string, uint64, int) ([]store.RuntimeEvent, error) {
	return nil, nil
}

func TestTerminalSessionIDFailureIsReturned(t *testing.T) {
	svc := &fakeTerminalSessionService{}
	want := errors.New("random source unavailable")
	sessions := &terminalSessions{sessions: svc, newID: func() (string, error) { return "", want }}
	if _, err := sessions.Create(context.Background(), "owner"); !errors.Is(err, want) {
		t.Fatalf("Create error = %v, want %v", err, want)
	}
	if svc.created != nil {
		t.Fatal("session was created after ID generation failed")
	}
}

func TestForwardInterrupts(t *testing.T) {
	signals := make(chan os.Signal, 1)
	forwarded, stop := forwardInterrupts(context.Background(), signals)
	signals <- os.Interrupt
	select {
	case <-forwarded:
	case <-time.After(time.Second):
		t.Fatal("interrupt was not forwarded")
	}
	stop()
}

func TestShouldUseTTY(t *testing.T) {
	cases := []struct {
		name          string
		present       chatPresentation
		inTTY, outTTY bool
		want          bool
	}{
		{"both terminals", chatPresentation{}, true, true, true},
		{"plain flag forces plain", chatPresentation{plain: true}, true, true, false},
		{"stdin not a terminal", chatPresentation{}, false, true, false},
		{"stdout not a terminal", chatPresentation{}, true, false, false},
		{"no color still streams", chatPresentation{noColor: true}, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldUseTTY(tc.present, tc.inTTY, tc.outTTY); got != tc.want {
				t.Errorf("shouldUseTTY(%+v, %v, %v) = %v, want %v", tc.present, tc.inTTY, tc.outTTY, got, tc.want)
			}
		})
	}
}

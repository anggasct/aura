package app

import (
	"context"
	"os"
	"testing"
	"time"
)

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

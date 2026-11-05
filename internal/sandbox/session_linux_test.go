package sandbox

import (
	"testing"
	"time"
)

func TestSetupWaitUsesTheSetupTimeoutIndependentlyOfTheExchangeTimeout(t *testing.T) {
	cases := []struct {
		name     string
		exchange time.Duration
		setup    time.Duration
		want     time.Duration
	}{
		{"setup timeout longer than the exchange timeout", 2 * time.Second, 30 * time.Second, 30 * time.Second},
		{"setup timeout shorter than the default wait", time.Minute, 3 * time.Second, 3 * time.Second},
		{"no setup timeout keeps the exchange-bounded default", 2 * time.Second, 0, 2 * time.Second},
		{"no setup timeout caps a long exchange at the default wait", time.Minute, 0, sessionSetupWait},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &SessionRequest{Limits: Limits{Timeout: tc.exchange}, SetupTimeout: tc.setup}
			if got := setupWait(req); got != tc.want {
				t.Fatalf("setupWait = %v, want %v", got, tc.want)
			}
		})
	}
}

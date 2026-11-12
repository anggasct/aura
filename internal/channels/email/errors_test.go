package email

import (
	"errors"
	"testing"

	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func TestDeliveryErrorsFulfilThePortContract(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		ambiguous bool
	}{
		{name: "an outcome nobody knows", err: Errorf(ErrorCodeDeliveryAmbiguous, "delivery outcome is unknown"), ambiguous: true},
		{name: "a refused delivery", err: Errorf(ErrorCodeDeliveryFailed, "delivery settled without success"), ambiguous: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errors.Is(tc.err, runtimechannelhost.ErrDeliveryAmbiguous); got != tc.ambiguous {
				t.Errorf("errors.Is(%v, ErrDeliveryAmbiguous) = %v, want %v", tc.err, got, tc.ambiguous)
			}
		})
	}
}

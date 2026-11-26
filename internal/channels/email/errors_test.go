package email

import (
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func TestDeliveryErrorsFulfilThePortContract(t *testing.T) {
	cause := errors.New("write failed mid-stream")
	cases := []struct {
		name      string
		err       error
		code      ErrorCode
		ambiguous bool
		wantCause error
	}{
		{name: "an outcome nobody knows", err: deliveryAmbiguous("delivery outcome is unknown", nil), code: ErrorCodeDeliveryAmbiguous, ambiguous: true},
		{name: "an ambiguous outcome with a cause", err: deliveryAmbiguous("submission outcome is unknown", cause), code: ErrorCodeDeliveryAmbiguous, ambiguous: true, wantCause: cause},
		{name: "a refused delivery", err: fault.New(ErrorCodeDeliveryFailed, "delivery settled without success"), code: ErrorCodeDeliveryFailed, ambiguous: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !fault.HasCode(tc.err, tc.code) {
				t.Errorf("fault.HasCode(%v, %q) = false, want true", tc.err, tc.code)
			}
			if got := errors.Is(tc.err, runtimechannelhost.ErrDeliveryAmbiguous); got != tc.ambiguous {
				t.Errorf("errors.Is(%v, ErrDeliveryAmbiguous) = %v, want %v", tc.err, got, tc.ambiguous)
			}
			if tc.wantCause != nil && !errors.Is(tc.err, tc.wantCause) {
				t.Errorf("errors.Is(%v, cause) = false, want true", tc.err)
			}
		})
	}
}

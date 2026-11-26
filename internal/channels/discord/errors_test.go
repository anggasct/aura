package discord

import (
	"errors"
	"fmt"
	"testing"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func TestErrorMatchesApprovalAndDeliverySentinels(t *testing.T) {
	cases := []struct {
		name        string
		code        ErrorCode
		wantUnavail bool
		wantTimeout bool
		wantAmbig   bool
	}{
		{name: "approval unavailable", code: ErrorCodeApprovalUnavailable, wantUnavail: true},
		{name: "approval timeout", code: ErrorCodeApprovalTimeout, wantTimeout: true},
		{name: "delivery ambiguous", code: ErrorCodeDeliveryAmbiguous, wantAmbig: true},
		{name: "unrelated connection failed", code: ErrorCodeConnectionFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", Errorf(tc.code, "detail"))
			if got := errors.Is(err, approval.ErrUnavailable); got != tc.wantUnavail {
				t.Errorf("errors.Is(%s, approval.ErrUnavailable) = %v, want %v", tc.code, got, tc.wantUnavail)
			}
			if got := errors.Is(err, approval.ErrTimeout); got != tc.wantTimeout {
				t.Errorf("errors.Is(%s, approval.ErrTimeout) = %v, want %v", tc.code, got, tc.wantTimeout)
			}
			if got := errors.Is(err, runtimechannelhost.ErrDeliveryAmbiguous); got != tc.wantAmbig {
				t.Errorf("errors.Is(%s, ErrDeliveryAmbiguous) = %v, want %v", tc.code, got, tc.wantAmbig)
			}
		})
	}
}

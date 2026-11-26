package discord

import (
	"errors"
	"fmt"
	"testing"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	runtimechannelhost "github.com/anggasct/aura/internal/platform/turn/channelhost"
)

func TestErrorMatchesApprovalAndDeliverySentinels(t *testing.T) {
	cases := []struct {
		name        string
		code        ErrorCode
		build       func(string) error
		wantUnavail bool
		wantTimeout bool
		wantAmbig   bool
	}{
		{name: "approval unavailable", code: ErrorCodeApprovalUnavailable, build: approvalUnavailable, wantUnavail: true},
		{name: "approval timeout", code: ErrorCodeApprovalTimeout, build: approvalTimeout, wantTimeout: true},
		{name: "delivery ambiguous", code: ErrorCodeDeliveryAmbiguous, build: deliveryAmbiguous, wantAmbig: true},
		{name: "unrelated connection failed", code: ErrorCodeConnectionFailed, build: func(detail string) error { return fault.New(ErrorCodeConnectionFailed, detail) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", tc.build("detail"))
			if !fault.HasCode(err, tc.code) {
				t.Errorf("fault.HasCode(%v, %s) = false, want true", err, tc.code)
			}
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

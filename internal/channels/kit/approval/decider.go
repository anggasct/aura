package approval

import (
	"context"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type Channel interface {
	HoldsApprovalTarget(sessionID string) bool
	Decide(ctx context.Context, prompt *Prompt) (bool, error)
}

func Decider(channels []Channel) toolbroker.ApprovalDecider {
	if len(channels) == 0 {
		return nil
	}
	return func(ctx context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
		if prompt == nil {
			return false, fault.New(ErrorCodeInvalidArgument, "approval prompt must not be nil")
		}
		for _, channel := range channels {
			if !channel.HoldsApprovalTarget(prompt.SessionID) {
				continue
			}
			return channel.Decide(ctx, &Prompt{
				ToolName:             prompt.ToolName,
				ToolVersion:          prompt.ToolVersion,
				SessionID:            prompt.SessionID,
				TurnID:               prompt.TurnID,
				PrincipalID:          prompt.PrincipalID,
				Arguments:            prompt.Arguments,
				Network:              prompt.Network,
				Timeout:              prompt.Timeout,
				MaxOutputBytes:       prompt.MaxOutputBytes,
				PolicyVersion:        prompt.PolicyVersion,
				ReasonCode:           prompt.ReasonCode,
				ExpiresAt:            prompt.ExpiresAt,
				StandingGrantAllowed: prompt.StandingGrantAllowed,
			})
		}
		return false, nil
	}
}

func Reachable(channels []Channel) func(sessionID string) bool {
	if len(channels) == 0 {
		return nil
	}
	return func(sessionID string) bool {
		for _, channel := range channels {
			if channel.HoldsApprovalTarget(sessionID) {
				return true
			}
		}
		return false
	}
}

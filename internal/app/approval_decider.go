package app

import (
	"context"
	"errors"

	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

type approvalChannel interface {
	HoldsApprovalTarget(sessionID string) bool
	Decide(ctx context.Context, prompt *approval.Prompt) (bool, error)
}

func channelApprovalDecider(channels []approvalChannel) toolbroker.ApprovalDecider {
	if len(channels) == 0 {
		return nil
	}
	return func(ctx context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
		if prompt == nil {
			return false, errors.New("approval prompt must not be nil")
		}
		for _, channel := range channels {
			if !channel.HoldsApprovalTarget(prompt.SessionID) {
				continue
			}
			return channel.Decide(ctx, &approval.Prompt{
				ToolName:       prompt.ToolName,
				ToolVersion:    prompt.ToolVersion,
				SessionID:      prompt.SessionID,
				TurnID:         prompt.TurnID,
				PrincipalID:    prompt.PrincipalID,
				Arguments:      prompt.Arguments,
				Network:        prompt.Network,
				Timeout:        prompt.Timeout,
				MaxOutputBytes: prompt.MaxOutputBytes,
				PolicyVersion:  prompt.PolicyVersion,
				ReasonCode:     prompt.ReasonCode,
				ExpiresAt:      prompt.ExpiresAt,

				StandingGrantAllowed: prompt.StandingGrantAllowed,
			})
		}
		return false, nil
	}
}

func channelApprovalReachable(channels []approvalChannel) func(sessionID string) bool {
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

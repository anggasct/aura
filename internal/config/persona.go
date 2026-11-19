package config

import (
	"errors"
	"maps"
	"time"

	"github.com/anggasct/aura/internal/platform/configkit"
)

const MaxSoulBytes = 32 << 10

const MaxPersonaNameRunes = 64

const MaxPersonaMessageRunes = 500

const PairingCodePlaceholder = "{code}"

const (
	AudienceOwner   = "owner"
	AudienceTrusted = "trusted"
	AudiencePublic  = "public"
)

const (
	GroupContextPerPrincipal = "per_principal"
	GroupContextShared       = "shared"
)

const (
	MinApprovalTimeout       = time.Minute
	MaxApprovalTimeout       = 24 * time.Hour
	MaxStandingGrantTTL      = 720 * time.Hour
	MinRetention             = 24 * time.Hour
	MessagePairingCode       = "pairing_code"
	MessagePairingPending    = "pairing_pending"
	MessagePairingLocked     = "pairing_locked"
	MessageWaitingForOwner   = "waiting_for_owner"
	MessageApprovalTimeout   = "approval_timeout"
	MessageApprovalDenied    = "approval_denied"
	MessageBudgetExhausted   = "budget_exhausted"
	MessageRateLimited       = "rate_limited"
	MessageNotPermitted      = "not_permitted"
	MessageHandoffOpen       = "handoff_open"
	MessageHandoffClosed     = "handoff_closed"
	MessageForgetmeConfirm   = "forgetme_confirm"
	MessageForgetmeDone      = "forgetme_done"
	MessageForgetmeStopping  = "forgetme_stopping"
	MessageUnavailable       = "unavailable"
	MessageWorkflowSucceeded = "workflow_succeeded"
	MessageWorkflowFailed    = "workflow_failed"
	MessageWorkflowCancelled = "workflow_cancelled"
	WorkflowGoalPlaceholder  = "{goal}"
)

var personaMessageDefaults = map[string]string{
	MessagePairingCode:       "To talk with me, ask the owner to approve this pairing code: " + PairingCodePlaceholder,
	MessagePairingPending:    "A pairing request is already pending. Please wait for the owner.",
	MessagePairingLocked:     "Too many pairing attempts. Please try again later.",
	MessageWaitingForOwner:   "One moment, I am checking with the owner.",
	MessageApprovalTimeout:   "The owner did not respond in time, so the request was cancelled. Please try again later.",
	MessageApprovalDenied:    "The owner declined this request.",
	MessageBudgetExhausted:   "Your daily quota is used up. Please try again tomorrow.",
	MessageRateLimited:       "You are sending messages too quickly. Please wait a moment.",
	MessageNotPermitted:      "That is not available here.",
	MessageHandoffOpen:       "I have passed this conversation to the owner, who will reply here.",
	MessageHandoffClosed:     "The owner has handed the conversation back to me.",
	MessageForgetmeConfirm:   "This erases your conversations and data with me. Send /forgetme confirm to proceed.",
	MessageForgetmeDone:      "Your data has been erased.",
	MessageForgetmeStopping:  "Your workflow runs are being stopped. Please send /forgetme confirm again in a minute.",
	MessageUnavailable:       "This assistant is not available right now.",
	MessageWorkflowSucceeded: "Done: " + WorkflowGoalPlaceholder,
	MessageWorkflowFailed:    "Sorry, " + WorkflowGoalPlaceholder + " could not be completed.",
	MessageWorkflowCancelled: WorkflowGoalPlaceholder + " was cancelled.",
}

func PersonaMessageDefaults() map[string]string {
	return maps.Clone(personaMessageDefaults)
}

func ValidateAccess(access *Access) error {
	if access == nil {
		return nil
	}
	var problems []error
	if timeout := time.Duration(access.ApprovalTimeout); timeout < MinApprovalTimeout || timeout > MaxApprovalTimeout {
		problems = append(problems, configkit.Invalid("access.approval_timeout", "must be between 1m and 24h"))
	}
	if ttl := time.Duration(access.StandingGrantTTL); ttl <= 0 || ttl > MaxStandingGrantTTL {
		problems = append(problems, configkit.Invalid("access.standing_grant_ttl", "must be positive and at most 720h"))
	}
	if timeout := time.Duration(access.HandoffTimeout); timeout < MinApprovalTimeout || timeout > MaxStandingGrantTTL {
		problems = append(problems, configkit.Invalid("access.handoff_timeout", "must be between 1m and 720h"))
	}
	for _, retention := range []struct {
		class string
		ttl   configkit.Duration
	}{{AudienceTrusted, access.Retention.Trusted}, {AudiencePublic, access.Retention.Public}} {
		if time.Duration(retention.ttl) < MinRetention {
			problems = append(problems, configkit.Invalid("access.retention."+retention.class, "must be at least 24h"))
		}
	}
	for _, budget := range []struct {
		class  string
		limits PrincipalBudget
	}{{AudienceTrusted, access.Budgets.Trusted}, {AudiencePublic, access.Budgets.Public}} {
		if budget.limits.MaxTokensPerDay < 1 {
			problems = append(problems, configkit.Invalid("access.budgets."+budget.class+".max_tokens_per_day", "must be at least 1"))
		}
		if budget.limits.MaxTurnsPerMinute < 1 {
			problems = append(problems, configkit.Invalid("access.budgets."+budget.class+".max_turns_per_minute", "must be at least 1"))
		}
	}
	return errors.Join(problems...)
}

package approval

import (
	"cmp"
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	policy "github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

const workflowSessionPrefix = "wf-"

var toolActionMessageKeys = map[string]string{
	"exec":             auraagent.MessageApprovalActionExec,
	"code_execute":     auraagent.MessageApprovalActionCodeExecute,
	"write_file":       auraagent.MessageApprovalActionWriteFile,
	"github_create_pr": auraagent.MessageApprovalActionGitHubCreate,
	"github_merge":     auraagent.MessageApprovalActionGitHubMerge,
	"github_comment":   auraagent.MessageApprovalActionGitHubComment,
	"email_send":       auraagent.MessageApprovalActionEmailSend,
}

type CardOptions struct {
	Persona        *auraagent.Definition
	FallbackName   string
	InlineCodeTool bool
	Clean          func(string) string
}

type RenderedCard struct {
	Header  string
	Details string
	Text    string
}

type CardButtons struct {
	Approve  string
	Reject   string
	Standing string
}

type CardOutcome string

const (
	CardOutcomeApproved   CardOutcome = "approved"
	CardOutcomeAllowedFor CardOutcome = "allowed_for"
	CardOutcomeRejected   CardOutcome = "rejected"
	CardOutcomeExpired    CardOutcome = "expired"
	CardOutcomeCancelled  CardOutcome = "cancelled"
)

type OutcomeLineInput struct {
	Persona     *auraagent.Definition
	Outcome     CardOutcome
	StandingTTL time.Duration
	At          time.Time
	Location    *time.Location
	Clean       func(string) string
}

func RenderCard(prompt *Prompt, opts CardOptions) RenderedCard {
	if prompt == nil {
		return RenderedCard{}
	}
	var def auraagent.Definition
	if opts.Persona != nil {
		def = *opts.Persona
	}
	clean := opts.Clean
	if clean == nil {
		clean = func(text string) string { return text }
	}
	escape := func(text string) string { return clean(tools.EscapeDisplayText(text)) }
	personaName := cmp.Or(def.Name, def.ID, opts.FallbackName, auraagent.DefaultName)

	var action string
	if key, known := toolActionMessageKeys[prompt.ToolName]; known {
		action = clean(def.Message(key))
	} else {
		tool := escape(prompt.ToolName)
		if opts.InlineCodeTool {
			tool = inlineCode(tool)
		}
		action = clean(def.FormatMessage(auraagent.MessageApprovalActionGeneric, auraagent.ToolPlaceholder, tool))
	}

	lines := make([]string, 0, 5)
	lines = append(lines, clean(def.FormatMessage(
		auraagent.MessageApprovalSummary,
		auraagent.PersonaPlaceholder, escape(personaName),
		auraagent.ActionPlaceholder, action,
	)))
	if prompt.PrincipalID != "" && prompt.PrincipalID != principal.OwnerID {
		lines = append(lines, clean(def.FormatMessage(
			auraagent.MessageApprovalRequestedBy,
			auraagent.RequesterPlaceholder, escape(prompt.PrincipalID),
		)))
	}
	if runID, ok := strings.CutPrefix(prompt.SessionID, workflowSessionPrefix); ok && runID != "" {
		lines = append(lines, clean(def.FormatMessage(
			auraagent.MessageApprovalWorkflowRun,
			auraagent.RunPlaceholder, escape(runID),
		)))
	}
	switch prompt.ReasonCode {
	case policy.ReasonUntrustedTrustGrant:
		lines = append(lines, clean(policy.UntrustedTrustGrantNotice))
	case policy.ReasonUntrustedInput:
		lines = append(lines, clean(def.Message(auraagent.MessageApprovalUntrustedInput)))
	case policy.ReasonContentUnverified:
		lines = append(lines, clean(policy.ContentUnverifiedNotice))
	}
	if prompt.TaintSource != "" {
		lines = append(lines, clean("came in through: "+escape(prompt.TaintSource)))
	}

	toolLine := escape(prompt.ToolName)
	if prompt.ToolVersion != "" {
		toolLine += "@" + escape(prompt.ToolVersion)
	}
	arguments := clean(strings.TrimSpace(prompt.Arguments))
	if arguments == "" {
		arguments = "{}"
	}
	details := toolLine + "\n" + arguments
	header := strings.Join(lines, "\n")
	return RenderedCard{
		Header:  header,
		Details: details,
		Text:    header + "\n" + details,
	}
}

func inlineCode(text string) string {
	return "`" + strings.ReplaceAll(text, "`", "`\u200b") + "`"
}

func FormatDuration(persona *auraagent.Definition, ttl time.Duration) string {
	var def auraagent.Definition
	if persona != nil {
		def = *persona
	}
	switch {
	case ttl == 24*time.Hour:
		return def.Message(auraagent.MessageDurationDay)
	case ttl > 0 && ttl%(24*time.Hour) == 0:
		return def.FormatMessage(auraagent.MessageDurationDays, auraagent.NumberPlaceholder, strconv.FormatInt(int64(ttl/(24*time.Hour)), 10))
	case ttl > 0 && ttl%time.Hour == 0:
		return def.FormatMessage(auraagent.MessageDurationHours, auraagent.NumberPlaceholder, strconv.FormatInt(int64(ttl/time.Hour), 10))
	default:
		return ttl.String()
	}
}

func RenderButtons(persona *auraagent.Definition, standingTTL time.Duration) CardButtons {
	var def auraagent.Definition
	if persona != nil {
		def = *persona
	}
	buttons := CardButtons{
		Approve: def.Message(auraagent.MessageButtonApprove),
		Reject:  def.Message(auraagent.MessageButtonReject),
	}
	if standingTTL > 0 {
		duration := tools.EscapeDisplayText(FormatDuration(&def, standingTTL))
		buttons.Standing = def.FormatMessage(auraagent.MessageButtonAllowFor, auraagent.DurationPlaceholder, duration)
	}
	return buttons
}

func FormatCardTime(at time.Time, loc *time.Location) string {
	if loc == nil {
		return at.UTC().Format("15:04") + " UTC"
	}
	return at.In(loc).Format("15:04")
}

func RenderOutcomeLine(in *OutcomeLineInput) string {
	if in == nil {
		return ""
	}
	var def auraagent.Definition
	if in.Persona != nil {
		def = *in.Persona
	}
	clean := in.Clean
	if clean == nil {
		clean = func(text string) string { return text }
	}
	timeText := tools.EscapeDisplayText(FormatCardTime(in.At, in.Location))
	switch in.Outcome {
	case CardOutcomeAllowedFor:
		duration := tools.EscapeDisplayText(FormatDuration(&def, in.StandingTTL))
		return clean(def.FormatMessage(
			auraagent.MessageCardAllowedFor,
			auraagent.DurationPlaceholder, duration,
			auraagent.TimePlaceholder, timeText,
		))
	case CardOutcomeApproved:
		return clean(def.FormatMessage(auraagent.MessageCardApproved, auraagent.TimePlaceholder, timeText))
	case CardOutcomeRejected:
		return clean(def.FormatMessage(auraagent.MessageCardRejected, auraagent.TimePlaceholder, timeText))
	case CardOutcomeExpired:
		return clean(def.FormatMessage(auraagent.MessageCardExpired, auraagent.TimePlaceholder, timeText))
	default:
		return clean(def.FormatMessage(auraagent.MessageCardCancelled, auraagent.TimePlaceholder, timeText))
	}
}

func CardOutcomeOf(outcome Outcome, timedOut bool, err error) CardOutcome {
	switch {
	case timedOut || errors.Is(err, context.DeadlineExceeded):
		return CardOutcomeExpired
	case err != nil:
		return CardOutcomeCancelled
	case outcome.Standing:
		return CardOutcomeAllowedFor
	case outcome.Accepted:
		return CardOutcomeApproved
	default:
		return CardOutcomeRejected
	}
}

func LongestOutcomeLine(in *OutcomeLineInput, includeStanding bool, measure func(string) int) int {
	if in == nil || measure == nil {
		return 0
	}
	sample := *in
	sample.At = time.Date(2000, 1, 1, 23, 59, 0, 0, time.UTC)
	outcomes := []CardOutcome{
		CardOutcomeApproved,
		CardOutcomeRejected,
		CardOutcomeExpired,
		CardOutcomeCancelled,
	}
	if includeStanding {
		outcomes = append(outcomes, CardOutcomeAllowedFor)
	}
	longest := 0
	for _, outcome := range outcomes {
		sample.Outcome = outcome
		longest = max(longest, measure(RenderOutcomeLine(&sample)))
	}
	return longest
}

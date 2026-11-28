package turn

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"

	"github.com/anggasct/aura/internal/platform/tools"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

type PartSource = runtimeingress.PartSource

const (
	PartSourceRelayed    = runtimeingress.PartSourceRelayed
	PartSourceAttachment = runtimeingress.PartSourceAttachment
)

type TaintSource string

const (
	TaintSourceOrigin         TaintSource = "origin"
	TaintSourceClientHistory  TaintSource = "client_history"
	TaintSourceWorkflowInput  TaintSource = "workflow_input"
	TaintSourceScheduleOrigin TaintSource = "schedule_origin"
	TaintSourceWebOnly        TaintSource = "web_only"
	TaintSourceKnowledgeScope TaintSource = "knowledge_scope"
	TaintSourceRelayedInput   TaintSource = "relayed_input"
	TaintSourceAttachment     TaintSource = "attachment"
	TaintSourceOtherPrincipal TaintSource = "other_principal"
	TaintSourceSession        TaintSource = "session"
	TaintSourceKnowledge      TaintSource = "knowledge"
	TaintSourceRecall         TaintSource = "recall"
	TaintSourceSkill          TaintSource = "skill"
	TaintSourceWorkspace      TaintSource = "workspace"
	TaintSourceToolResult     TaintSource = "tool_result"
	TaintSourceLegacy         TaintSource = "legacy"
)

const MaxTaintDetailBytes = 120

type TaintMark struct {
	Source     TaintSource
	Detail     string
	Principal  string
	AtSequence int64
}

func (m TaintMark) Label() string {
	return boundedDetail(m.label())
}

func (m TaintMark) label() string {
	switch m.Source {
	case TaintSourceOrigin:
		if Origin(m.Detail).OnChannel(string(OriginWebhook)) {
			return "a webhook"
		}
		if m.Detail == "" {
			return "a turn from an unknown origin"
		}
		return "a turn started by " + tools.EscapeDisplayText(m.Detail)
	case TaintSourceClientHistory:
		return "HTTP client history"
	case TaintSourceWorkflowInput:
		return "a workflow step"
	case TaintSourceScheduleOrigin:
		return "a schedule created in a conversation with untrusted content"
	case TaintSourceWebOnly:
		return "a web-only scheduled run"
	case TaintSourceKnowledgeScope:
		return "knowledge attached to this persona"
	case TaintSourceRelayedInput:
		return "a forwarded or quoted message"
	case TaintSourceAttachment:
		return "an attachment"
	case TaintSourceOtherPrincipal:
		return "a message from someone else in this conversation"
	case TaintSourceSession:
		inner := TaintSource(m.Detail)
		if inner == "" || inner == TaintSourceSession {
			return "earlier in this conversation: content from outside your control"
		}
		return "earlier in this conversation: " + TaintMark{Source: inner}.label()
	case TaintSourceKnowledge:
		if m.Detail == "" {
			return "knowledge passages"
		}
		return "knowledge passages from " + tools.EscapeDisplayText(m.Detail)
	case TaintSourceRecall:
		return "recalled memory"
	case TaintSourceSkill:
		return "a skill accepted in a conversation with untrusted content"
	case TaintSourceWorkspace:
		return "a workspace file written while a conversation held untrusted content"
	case TaintSourceToolResult:
		return toolResultLabel(m.Detail)
	case TaintSourceLegacy:
		return "content this conversation held before taint tracking"
	default:
		return "content from outside your control"
	}
}

func toolResultLabel(detail string) string {
	toolName, host, hasHost := strings.Cut(strings.TrimSpace(detail), " ")
	if toolName == "" {
		return "a tool result"
	}
	shownTool := tools.EscapeDisplayText(toolName)
	if toolName == "web_fetch" && hasHost {
		host = strings.TrimSpace(host)
		if ascii, err := idna.Lookup.ToASCII(host); err == nil && ascii != "" {
			return "a web_fetch result from " + tools.EscapeDisplayText(strings.ToLower(ascii))
		}
		return "a web_fetch result"
	}
	return "a " + shownTool + " result"
}

func boundedDetail(detail string) string {
	if len(detail) <= MaxTaintDetailBytes {
		return detail
	}
	cut := MaxTaintDetailBytes
	for cut > 0 && !utf8.RuneStart(detail[cut]) {
		cut--
	}
	return strings.ToValidUTF8(detail[:cut], "")
}

func (r *TurnRequest) AdmissionTaint() (TaintMark, bool) {
	switch {
	case r == nil:
		return TaintMark{Source: TaintSourceOrigin}, true
	case !authoredInputOrigin(r.Origin):
		return TaintMark{Source: TaintSourceOrigin, Detail: boundedDetail(string(r.Origin))}, true
	case r.UntrustedContext != nil:
		return TaintMark{Source: TaintSourceClientHistory}, true
	case r.InputTainted:
		return TaintMark{Source: TaintSourceWorkflowInput}, true
	case r.JobTaint != "":
		return TaintMark{Source: TaintSourceScheduleOrigin, Detail: boundedDetail(r.JobID)}, true
	case r.WebOnly:
		return TaintMark{Source: TaintSourceWebOnly, Detail: boundedDetail(r.JobID)}, true
	case len(r.Knowledge) > 0:
		return TaintMark{Source: TaintSourceKnowledgeScope}, true
	}
	for i := range r.Parts {
		if r.Parts[i].Source == PartSourceRelayed {
			return TaintMark{Source: TaintSourceRelayedInput, Detail: boundedDetail(r.Parts[i].Detail)}, true
		}
	}
	return TaintMark{}, false
}

func WebOnlyRestriction() *ToolRestriction {
	return &ToolRestriction{Allowed: []string{"web_search", "web_fetch"}}
}

package turn

import (
	"strings"
	"unicode/utf8"
)

type TaintSource string

const (
	TaintSourceOrigin         TaintSource = "origin"
	TaintSourceClientHistory  TaintSource = "client_history"
	TaintSourceWorkflowInput  TaintSource = "workflow_input"
	TaintSourceScheduleOrigin TaintSource = "schedule_origin"
	TaintSourceWebOnly        TaintSource = "web_only"
	TaintSourceKnowledgeScope TaintSource = "knowledge_scope"
)

const MaxTaintDetailBytes = 120

type TaintMark struct {
	Source TaintSource
	Detail string
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
		return "a turn started by " + m.Detail
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
	}
	return "content from outside your control"
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
	return TaintMark{}, false
}

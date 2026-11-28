package turn

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/principal"
)

func TestNeutralizeUntrustedMarkers(t *testing.T) {
	tests := map[string]struct {
		input string
		want  string
	}{
		"plain text":            {input: "plain text", want: "plain text"},
		"unrelated bracket":     {input: "[note] untrusted", want: "[note] untrusted"},
		"recall start":          {input: RecallEvidenceStart + ": x]", want: "(untrusted recall evidence: x]"},
		"recall end":            {input: RecallEvidenceEnd, want: "(untrusted recall evidence]"},
		"profile start":         {input: ProfileEvidenceStart + "]", want: "(untrusted profile context]"},
		"profile end":           {input: ProfileEvidenceEnd, want: "(untrusted profile context]"},
		"skill start":           {input: SkillContextStart + "]", want: "(untrusted_skill_context]"},
		"skill end":             {input: SkillContextEnd, want: "(untrusted_skill_context]"},
		"nested closing":        {input: "[/untrusted [/untrusted x]x]", want: "(untrusted (untrusted x]x]"},
		"nested bracket":        {input: "[[/untrusted", want: "[(untrusted"},
		"case variant":          {input: "[/UnTrUsTeD recall evidence]", want: "(untrusted recall evidence]"},
		"whitespace padded":     {input: "[ \t/ \n untrusted recall evidence]", want: "(untrusted recall evidence]"},
		"newline after bracket": {input: "[\n/\nuntrusted_skill_context]", want: "(untrusted_skill_context]"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := NeutralizeUntrustedMarkers(test.input)
			if got != test.want {
				t.Errorf("NeutralizeUntrustedMarkers(%q) = %q, want %q", test.input, got, test.want)
			}
			if again := NeutralizeUntrustedMarkers(got); again != got {
				t.Errorf("not idempotent: %q then %q", got, again)
			}
		})
	}
}

func TestQuoteUntrustedEscapesNewlinesQuotesAndMarkersWithoutHTMLEscaping(t *testing.T) {
	tests := map[string]struct {
		input string
		want  string
	}{
		"plain":   {input: "a b", want: `"a b"`},
		"newline": {input: "a\nb", want: `"a\nb"`},
		"quote":   {input: `say "hi"`, want: `"say \"hi\""`},
		"html":    {input: "<b>&", want: `"<b>&"`},
		"marker":  {input: "[/untrusted_knowledge]", want: `"(untrusted_knowledge]"`},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := QuoteUntrusted(test.input); got != test.want {
				t.Errorf("QuoteUntrusted(%q) = %s, want %s", test.input, got, test.want)
			}
		})
	}
}

func TestTurnInputIsUntrustedUnlessAPrincipalAuthoredIt(t *testing.T) {
	tests := map[string]struct {
		req  *TurnRequest
		want bool
	}{
		"nil request":                 {req: nil, want: true},
		"terminal":                    {req: &TurnRequest{Origin: OriginTerminal}},
		"http":                        {req: &TurnRequest{Origin: OriginHTTP}},
		"schedule":                    {req: &TurnRequest{Origin: OriginSchedule}},
		"discord account":             {req: &TurnRequest{Origin: "discord:main"}},
		"telegram account":            {req: &TurnRequest{Origin: "telegram:main"}},
		"email account":               {req: &TurnRequest{Origin: "email:main"}},
		"workflow":                    {req: &TurnRequest{Origin: OriginWorkflow}},
		"workflow with tainted input": {req: &TurnRequest{Origin: OriginWorkflow, InputTainted: true}, want: true},
		"terminal with tainted input": {req: &TurnRequest{Origin: OriginTerminal, InputTainted: true}, want: true},
		"webhook":                     {req: &TurnRequest{Origin: OriginWebhook}, want: true},
		"webhook account":             {req: &TurnRequest{Origin: "webhook:main"}, want: true},
		"child":                       {req: &TurnRequest{Origin: OriginChild}, want: true},
		"internal":                    {req: &TurnRequest{Origin: OriginInternal}, want: true},
		"empty origin":                {req: &TurnRequest{}, want: true},
		"unlisted channel":            {req: &TurnRequest{Origin: "slack:main"}, want: true},
		"empty account":               {req: &TurnRequest{Origin: "discord:"}, want: true},
		"channel without colon":       {req: &TurnRequest{Origin: "discord"}, want: true},
		"terminal with knowledge": {
			req:  &TurnRequest{Origin: OriginTerminal, Knowledge: []string{"faq"}},
			want: true,
		},
		"http with empty context": {
			req:  &TurnRequest{Origin: OriginHTTP, UntrustedContext: &UntrustedRecall{}},
			want: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tc.req.Tainted(); got != tc.want {
				t.Errorf("Tainted() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStepOneAdmissionSourcesAndLabels(t *testing.T) {
	tests := map[string]struct {
		req        *TurnRequest
		wantSource TaintSource
		wantDetail string
		wantLabel  string
	}{
		"nil request":          {req: nil, wantSource: TaintSourceOrigin, wantLabel: "a turn from an unknown origin"},
		"webhook":              {req: &TurnRequest{Origin: OriginWebhook}, wantSource: TaintSourceOrigin, wantDetail: "webhook", wantLabel: "a webhook"},
		"webhook account":      {req: &TurnRequest{Origin: "webhook:main"}, wantSource: TaintSourceOrigin, wantDetail: "webhook:main", wantLabel: "a webhook"},
		"child":                {req: &TurnRequest{Origin: OriginChild}, wantSource: TaintSourceOrigin, wantDetail: "child", wantLabel: "a turn started by child"},
		"client history":       {req: &TurnRequest{Origin: OriginHTTP, UntrustedContext: &UntrustedRecall{}}, wantSource: TaintSourceClientHistory, wantLabel: "HTTP client history"},
		"workflow input":       {req: &TurnRequest{Origin: OriginWorkflow, InputTainted: true}, wantSource: TaintSourceWorkflowInput, wantLabel: "a workflow step"},
		"tainted job":          {req: &TurnRequest{Origin: OriginSchedule, JobTaint: "x", JobID: "job-1"}, wantSource: TaintSourceScheduleOrigin, wantDetail: "job-1", wantLabel: "a schedule created in a conversation with untrusted content"},
		"web only":             {req: &TurnRequest{Origin: OriginSchedule, WebOnly: true, JobID: "job-2"}, wantSource: TaintSourceWebOnly, wantDetail: "job-2", wantLabel: "a web-only scheduled run"},
		"knowledge":            {req: &TurnRequest{Origin: OriginTerminal, Knowledge: []string{"faq"}}, wantSource: TaintSourceKnowledgeScope, wantLabel: "knowledge attached to this persona"},
		"knowledge for owner":  {req: &TurnRequest{Origin: OriginTerminal, PrincipalClass: principal.ClassOwner, Knowledge: []string{"faq"}}, wantSource: TaintSourceKnowledgeScope, wantLabel: "knowledge attached to this persona"},
		"origin beats history": {req: &TurnRequest{Origin: OriginWebhook, UntrustedContext: &UntrustedRecall{}, InputTainted: true, WebOnly: true, Knowledge: []string{"a"}}, wantSource: TaintSourceOrigin, wantDetail: "webhook", wantLabel: "a webhook"},
		"history beats input":  {req: &TurnRequest{Origin: OriginHTTP, UntrustedContext: &UntrustedRecall{}, InputTainted: true, JobTaint: "x", WebOnly: true, Knowledge: []string{"a"}}, wantSource: TaintSourceClientHistory, wantLabel: "HTTP client history"},
		"input beats job":      {req: &TurnRequest{Origin: OriginSchedule, InputTainted: true, JobTaint: "x", WebOnly: true, Knowledge: []string{"a"}}, wantSource: TaintSourceWorkflowInput, wantLabel: "a workflow step"},
		"job beats web only":   {req: &TurnRequest{Origin: OriginSchedule, JobTaint: "x", JobID: "job-3", WebOnly: true, Knowledge: []string{"a"}}, wantSource: TaintSourceScheduleOrigin, wantDetail: "job-3", wantLabel: "a schedule created in a conversation with untrusted content"},
		"web only beats scope": {req: &TurnRequest{Origin: OriginSchedule, WebOnly: true, Knowledge: []string{"a"}}, wantSource: TaintSourceWebOnly, wantLabel: "a web-only scheduled run"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			mark, tainted := tc.req.AdmissionTaint()
			if !tainted {
				t.Fatal("AdmissionTaint() = clean, want tainted")
			}
			if mark.Source != tc.wantSource || mark.Detail != tc.wantDetail {
				t.Errorf("AdmissionTaint() = %+v, want source %q detail %q", mark, tc.wantSource, tc.wantDetail)
			}
			if got := mark.Label(); got != tc.wantLabel {
				t.Errorf("Label() = %q, want %q", got, tc.wantLabel)
			}
			if !tc.req.Tainted() {
				t.Error("Tainted() = false, want the boolean of AdmissionTaint()")
			}
		})
	}
	clean := map[string]*TurnRequest{
		"terminal":         {Origin: OriginTerminal},
		"schedule":         {Origin: OriginSchedule},
		"clean job":        {Origin: OriginSchedule, JobID: "job-1"},
		"discord account":  {Origin: "discord:main"},
		"workflow origin":  {Origin: OriginWorkflow},
		"job id but clean": {Origin: OriginSchedule, JobID: "job-9", Timezone: "UTC"},
	}
	for name, req := range clean {
		t.Run("clean "+name, func(t *testing.T) {
			if mark, tainted := req.AdmissionTaint(); tainted || mark != (TaintMark{}) {
				t.Errorf("AdmissionTaint() = %+v %v, want clean", mark, tainted)
			}
			if req.Tainted() {
				t.Error("Tainted() = true, want the boolean of AdmissionTaint()")
			}
		})
	}
}

func TestTaintLabelsStayWithinTheDetailBound(t *testing.T) {
	long := strings.Repeat("é", 100)
	req := &TurnRequest{Origin: Origin("slack:" + long)}
	mark, _ := req.AdmissionTaint()
	if len(mark.Detail) > MaxTaintDetailBytes || !utf8.ValidString(mark.Detail) {
		t.Errorf("detail is %d bytes, valid utf8 %v; want at most %d and valid", len(mark.Detail), utf8.ValidString(mark.Detail), MaxTaintDetailBytes)
	}
	if label := mark.Label(); len(label) > MaxTaintDetailBytes || !utf8.ValidString(label) {
		t.Errorf("label is %d bytes, valid utf8 %v; want at most %d and valid", len(label), utf8.ValidString(label), MaxTaintDetailBytes)
	}
}

func TestQuoteUntrustedNameKeepsTheNameInsideOneQuotedToken(t *testing.T) {
	tests := map[string]struct {
		input string
		want  string
	}{
		"plain":           {input: "policy.md", want: `"policy.md"`},
		"quotes":          {input: `notes "final" [v2].md`, want: `"notes \"final\" [v2].md"`},
		"marker":          {input: "[untrusted x] a", want: `"(untrusted x] a"`},
		"newline":         {input: "a\nb", want: `"a\nb"`},
		"bidi override":   {input: "a\u202eb", want: `"a\u202eb"`},
		"isolate control": {input: "a\u2066b", want: `"a\u2066b"`},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := QuoteUntrustedName(test.input); got != test.want {
				t.Errorf("QuoteUntrustedName(%q) = %s, want %s", test.input, got, test.want)
			}
		})
	}
}

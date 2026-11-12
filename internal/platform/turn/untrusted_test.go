package turn

import "testing"

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
		"nil request":           {req: nil, want: true},
		"terminal":              {req: &TurnRequest{Origin: OriginTerminal}},
		"http":                  {req: &TurnRequest{Origin: OriginHTTP}},
		"schedule":              {req: &TurnRequest{Origin: OriginSchedule}},
		"discord account":       {req: &TurnRequest{Origin: "discord:main"}},
		"telegram account":      {req: &TurnRequest{Origin: "telegram:main"}},
		"email account":         {req: &TurnRequest{Origin: "email:main"}},
		"webhook":               {req: &TurnRequest{Origin: OriginWebhook}, want: true},
		"webhook account":       {req: &TurnRequest{Origin: "webhook:main"}, want: true},
		"child":                 {req: &TurnRequest{Origin: OriginChild}, want: true},
		"internal":              {req: &TurnRequest{Origin: OriginInternal}, want: true},
		"empty origin":          {req: &TurnRequest{}, want: true},
		"unlisted channel":      {req: &TurnRequest{Origin: "slack:main"}, want: true},
		"empty account":         {req: &TurnRequest{Origin: "discord:"}, want: true},
		"channel without colon": {req: &TurnRequest{Origin: "discord"}, want: true},
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

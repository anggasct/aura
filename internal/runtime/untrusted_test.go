package runtime

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

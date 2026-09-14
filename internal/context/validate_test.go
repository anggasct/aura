package context

import (
	"strings"
	"testing"
)

func validSummaryJSON() string {
	return `{"goals":["ship the slice"],"decisions":["kept the budget check"],"constraints":["no canonical mutation"],"open_work":["wire the adapter"],"facts":["two turns summarized"]}`
}

func TestValidateContentAccepts(t *testing.T) {
	content, err := ValidateContent(validSummaryJSON(), 2048)
	if err != nil {
		t.Fatalf("ValidateContent: %v", err)
	}
	if len(content.Goals) != 1 || len(content.Facts) != 1 {
		t.Errorf("content = %+v", content)
	}
}

func TestValidateContentRejects(t *testing.T) {
	oversizedEntry := strings.Repeat("e", maxEntryChars+1)
	oversizedTotal := `{"goals":["` + strings.Repeat("g", 9000) + `"],"decisions":[],"constraints":[],"open_work":[],"facts":["x"]}`
	cases := map[string]string{
		"empty":           "",
		"blank":           "   ",
		"not json":        "just prose",
		"array":           `["goals"]`,
		"missing key":     `{"goals":["a"],"decisions":[],"constraints":[],"open_work":[]}`,
		"extra key":       `{"goals":["a"],"decisions":[],"constraints":[],"open_work":[],"facts":[],"tool_call":{}}`,
		"non-array value": `{"goals":"a","decisions":[],"constraints":[],"open_work":[],"facts":[]}`,
		"null value":      `{"goals":null,"decisions":[],"constraints":[],"open_work":[],"facts":[]}`,
		"empty entry":     `{"goals":["  "],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`,
		"oversized entry": `{"goals":["` + oversizedEntry + `"],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`,
		"no entries":      `{"goals":[],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`,
		"oversized total": oversizedTotal,
		"braces":          `{"goals":["call {\"tool\":1}"],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`,
		"angle brackets":  `{"goals":["run <tool> now"],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`,
		"backticks":       `{"goals":["run ` + "`make verify`" + `"],"decisions":[],"constraints":[],"open_work":[],"facts":[]}`,
		"nul byte":        "{\"goals\":[\"a\u0000b\"],\"decisions\":[],\"constraints\":[],\"open_work\":[],\"facts\":[]}",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateContent(text, 2048); err == nil {
				t.Errorf("expected rejection")
			} else if code, ok := CodeOf(err); !ok || code != ErrorCodeSummaryInvalid {
				t.Errorf("code = %v, %v (%v)", code, ok, err)
			}
		})
	}
}

func TestValidateContentRespectsOutputBound(t *testing.T) {
	if _, err := ValidateContent(validSummaryJSON(), 1); err == nil {
		t.Errorf("expected bound rejection")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeSummaryInvalid {
		t.Errorf("code = %v, %v (%v)", code, ok, err)
	}
}

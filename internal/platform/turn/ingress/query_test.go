package runtimeingress

import (
	"strings"
	"testing"
)

func TestQueryTextJoinsTrimmedTextPartsAndBoundsTheRunes(t *testing.T) {
	tests := map[string]struct {
		parts []InputPart
		want  string
	}{
		"none":           {parts: nil, want: ""},
		"blank parts":    {parts: []InputPart{{Text: "  "}, {Text: "\n"}}, want: ""},
		"image only":     {parts: []InputPart{{Image: []byte(`{"a":1}`)}}, want: ""},
		"joined":         {parts: []InputPart{{Text: " first "}, {Image: []byte(`{}`)}, {Text: "second\n"}}, want: "first\nsecond"},
		"bounded":        {parts: []InputPart{{Text: strings.Repeat("é", MaxQueryRunes+40)}}, want: strings.Repeat("é", MaxQueryRunes)},
		"exact boundary": {parts: []InputPart{{Text: strings.Repeat("a", MaxQueryRunes)}}, want: strings.Repeat("a", MaxQueryRunes)},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := QueryText(test.parts); got != test.want {
				t.Errorf("QueryText = %q, want %q", got, test.want)
			}
		})
	}
}

func TestJoinedTextKeepsEveryRuneThatQueryTextBounds(t *testing.T) {
	long := strings.Repeat("é", MaxQueryRunes+40)
	parts := []InputPart{{Text: " first "}, {Text: long}}
	if got, want := JoinedText(parts), "first\n"+long; got != want {
		t.Errorf("JoinedText kept %d runes, want %d", len([]rune(got)), len([]rune(want)))
	}
	if got := QueryText(parts); len([]rune(got)) != MaxQueryRunes || !strings.HasPrefix(got, "first\n") {
		t.Errorf("QueryText = %d runes, want %d", len([]rune(got)), MaxQueryRunes)
	}
	if got := JoinedText(nil); got != "" {
		t.Errorf("JoinedText(nil) = %q", got)
	}
}

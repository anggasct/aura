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

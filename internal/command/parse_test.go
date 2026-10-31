package command

import (
	"slices"
	"strings"
	"testing"
)

func TestParseRecognizesOnlyLeadingCommandTokens(t *testing.T) {
	tests := []struct {
		text string
		ok   bool
		name string
		args []string
	}{
		{"/help", true, "help", nil},
		{"/HELP", true, "help", nil},
		{"  /Skill-Create  n  a  b ", true, "skill-create", []string{"n", "a", "b"}},
		{"/session sess_1", true, "session", []string{"sess_1"}},
		{"/a" + strings.Repeat("b", 31), true, "a" + strings.Repeat("b", 31), nil},
		{"/a" + strings.Repeat("b", 32), false, "", nil},
		{"/", false, "", nil},
		{"/1abc", false, "", nil},
		{"/-abc", false, "", nil},
		{"/a_b", false, "", nil},
		{"/hélp", false, "", nil},
		{"/Kill", false, "", nil},
		{"//help", false, "", nil},
		{"hello /help", false, "", nil},
		{"<@123> /help", false, "", nil},
		{"", false, "", nil},
		{"   ", false, "", nil},
		{".", false, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			inv, ok := Parse(tt.text)
			if ok != tt.ok {
				t.Fatalf("Parse(%q) ok = %v, want %v", tt.text, ok, tt.ok)
			}
			if !ok {
				return
			}
			if inv.Name != tt.name || !slices.Equal(inv.Args, tt.args) {
				t.Errorf("Parse(%q) = %+v, want %s %v", tt.text, inv, tt.name, tt.args)
			}
			if inv.Token != strings.Fields(tt.text)[0] {
				t.Errorf("token = %q", inv.Token)
			}
		})
	}
}

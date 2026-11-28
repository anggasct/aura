package agent

import (
	"slices"
	"testing"

	"github.com/anggasct/aura/internal/platform/principal"
)

func TestListedToolsFollowTheClassCeiling(t *testing.T) {
	definition := &Definition{Tools: []string{"read_file", "web_search"}, PrincipalTools: []string{"read_file"}, PublicTools: nil}
	cases := map[string]struct {
		class   principal.Class
		want    []string
		listed  bool
		offered map[string]bool
	}{
		"the owner":           {principal.ClassOwner, []string{"read_file", "web_search"}, true, map[string]bool{"web_search": true, "web_fetch": false}},
		"a trusted principal": {principal.ClassTrusted, []string{"read_file"}, true, map[string]bool{"web_search": false, "read_file": true}},
		"a public caller":     {principal.ClassPublic, nil, false, map[string]bool{"web_search": false}},
		"no class":            {"", []string{"read_file", "web_search"}, true, map[string]bool{"web_search": true}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := definition.ListedTools(tc.class)
			if ok != tc.listed || !slices.Equal(got, tc.want) {
				t.Errorf("ListedTools = %v, %t, want %v, %t", got, ok, tc.want, tc.listed)
			}
			for tool, want := range tc.offered {
				if definition.OffersPlainTool(tc.class, tool) != want {
					t.Errorf("OffersPlainTool(%q) = %t, want %t", tool, !want, want)
				}
			}
		})
	}
	t.Run("a persona with no tool list offers every plain tool to its owner", func(t *testing.T) {
		if !(&Definition{}).OffersPlainTool(principal.ClassOwner, "web_fetch") {
			t.Error("an unlisted persona offered no web_fetch to its owner")
		}
	})
}

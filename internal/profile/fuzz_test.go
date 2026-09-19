package profile

import (
	"testing"
)

func FuzzParseCandidates(f *testing.F) {
	f.Add(`[{"category":"tool","key":"editor","value":"neovim","sources":[1]}]`)
	f.Add(`[]`)
	f.Add(`not json`)
	f.Add(`[{"category":"","key":"k","value":"v","sources":[]}]`)
	f.Add(`[{"category":"` + string(make([]byte, 512)) + `","key":"k","value":"v","sources":[1,2]}]`)
	f.Fuzz(func(t *testing.T, text string) {
		candidates, err := ParseCandidates(text)
		if err != nil {
			return
		}
		for _, candidate := range candidates {
			if candidate.Category == "" || candidate.Key == "" || candidate.Value == "" || len(candidate.Sources) == 0 {
				t.Fatalf("parsed candidate is incomplete: %+v", candidate)
			}
		}
	})
}

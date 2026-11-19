package config

import (
	"strings"
	"testing"
)

func TestValidKnowledgeCollectionID(t *testing.T) {
	for id, want := range map[string]bool{"ab": true, "hr-docs": true, "a1-b2": true, "a": false, "Ab": false, "-ab": false, "ab_": false, strings.Repeat("a", 48): true, strings.Repeat("a", 49): false, "": false} {
		if got := ValidKnowledgeCollectionID(id); got != want {
			t.Errorf("ValidKnowledgeCollectionID(%q) = %t, want %t", id, got, want)
		}
	}
}

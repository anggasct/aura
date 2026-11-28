package media

import (
	"strings"
	"testing"
)

func TestAttachmentReferenceQuotesFilename(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		want     string
	}{
		{name: "a plain name", filename: "policy.md", want: `[attachment "policy.md" text/markdown 12 bytes artifact:art-1]`},
		{name: "a quote and a bracket", filename: `a"] ignore.md`, want: `[attachment "a\"] ignore.md" text/markdown 12 bytes artifact:art-1]`},
		{name: "a newline", filename: "a\nb.md", want: `[attachment "a\nb.md" text/markdown 12 bytes artifact:art-1]`},
		{name: "a directory prefix", filename: "../../etc/passwd", want: `[attachment "passwd" text/markdown 12 bytes artifact:art-1]`},
		{name: "a right-to-left override", filename: "evil\u202Efdp.md", want: `[attachment "evil\u202efdp.md" text/markdown 12 bytes artifact:art-1]`},
		{name: "an empty name", filename: "", want: `[attachment "attachment" text/markdown 12 bytes artifact:art-1]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReferencePart(&ArtifactSummary{RefID: "art-1", SizeBytes: 12, Filename: tc.filename}, "text/markdown; charset=utf-8")

			if got != tc.want {
				t.Errorf("ReferencePart = %q, want %q", got, tc.want)
			}
			if strings.ContainsAny(got, "\n\r") {
				t.Errorf("ReferencePart %q spans lines", got)
			}
		})
	}
}

func TestAttachableAddsOnlyTheTextDocuments(t *testing.T) {
	cases := []struct {
		mediaType    string
		wantEligible bool
		wantAttach   bool
	}{
		{mediaType: "image/png", wantEligible: true, wantAttach: true},
		{mediaType: "application/pdf", wantEligible: true, wantAttach: true},
		{mediaType: "text/markdown", wantAttach: true},
		{mediaType: "TEXT/Plain; charset=utf-8", wantAttach: true},
		{mediaType: "text/html", wantAttach: true},
		{mediaType: "text/csv"},
		{mediaType: "text/x-python"},
		{mediaType: "application/zip"},
		{mediaType: ""},
	}
	for _, tc := range cases {
		t.Run(tc.mediaType, func(t *testing.T) {
			if got := Eligible(tc.mediaType); got != tc.wantEligible {
				t.Errorf("Eligible = %v, want %v", got, tc.wantEligible)
			}
			if got := Attachable(tc.mediaType); got != tc.wantAttach {
				t.Errorf("Attachable = %v, want %v", got, tc.wantAttach)
			}
		})
	}
}

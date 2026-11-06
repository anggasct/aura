package gmail

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzExtractBody(f *testing.F) {
	seeds := []string{
		`{"mimeType":"text/plain","body":{"data":"aGVsbG8"}}`,
		`{"mimeType":"multipart/mixed","parts":[{"mimeType":"text/html","body":{"data":"PHA-aGk8L3A-"}},{"mimeType":"application/pdf","filename":"a.pdf","body":{"size":5,"attachmentId":"x"}}]}`,
		`{"mimeType":"text/plain","headers":[{"name":"Content-Type","value":"text/plain; charset=shift_jis"}],"body":{"data":"gYGBgQ"}}`,
		`{"mimeType":"multipart/mixed","parts":[{"mimeType":"multipart/mixed","parts":[{"mimeType":"text/plain","body":{"data":"eA"}}]}]}`,
		`{}`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		var root part
		if json.Unmarshal(raw, &root) != nil {
			return
		}
		got := extractBody(&root)
		if len(got.text) > maxReadBody || !utf8.ValidString(got.text) || len(got.attachments) > maxAttachments {
			t.Fatalf("out of bounds: %d bytes, valid=%v, %d attachments", len(got.text), utf8.ValidString(got.text), len(got.attachments))
		}
		for _, r := range got.text {
			if r != '\n' && r != '\t' && unwanted(r) {
				t.Fatalf("kept %U", r)
			}
		}
	})
}

func FuzzHTMLText(f *testing.F) {
	for _, seed := range []string{
		`<p>hi</p>`, `<div style="display:none">x</div>y`, `<script>x</script>`, `<<<>>>`, `<p style=`, `<!-- x`, strings.Repeat("<div>", 600),
		`<head><title>t</title><body>b`, `&#0;&#x110000;&amp`, `<input type=hidden>`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, markup string) {
		text, _ := htmlText([]byte(markup))
		if len(text) > maxTextBytes+maxPartBytes {
			t.Fatalf("output of %d bytes", len(text))
		}
	})
}

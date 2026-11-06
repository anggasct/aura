package email

import (
	"fmt"
	"strings"
	"testing"
)

func withMIMEBody(spec mailSpec, headersAndBody string) []byte {
	head, _, _ := strings.Cut(string(spec.raw()), "MIME-Version: 1.0\r\n")
	return []byte(head + "MIME-Version: 1.0\r\n" + headersAndBody)
}

func textPart(boundary, body string) string {
	return "--" + boundary + "\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + body + "\r\n"
}

func imagePart(boundary string) string {
	return "--" + boundary + "\r\nContent-Type: image/png\r\nContent-Disposition: attachment; filename=\"p.png\"\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw0KGgo=\r\n"
}

func nestedMultipart(depth int) string {
	var out strings.Builder
	for level := range depth {
		fmt.Fprintf(&out, "Content-Type: multipart/mixed; boundary=L%d\r\n\r\n--L%d\r\n", level, level)
	}
	out.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\nhello\r\n")
	for level := depth - 1; level >= 0; level-- {
		fmt.Fprintf(&out, "--L%d--\r\n", level)
	}
	return out.String()
}

func TestAttachments_StructureErrorsFailClosed(t *testing.T) {
	manyParts := func(count int) string {
		var out strings.Builder
		out.WriteString("Content-Type: multipart/mixed; boundary=B\r\n\r\n")
		for i := range count {
			out.WriteString(textPart("B", fmt.Sprintf("part %d", i)))
		}
		out.WriteString("--B--\r\n")
		return out.String()
	}
	cases := []struct {
		name     string
		body     string
		wantDrop string
	}{
		{name: "truncated multipart boundary", body: "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + textPart("B", "hello") + "--B\r\nContent-Type: image/png\r\n\r\npartial", wantDrop: "attachment_failed"},
		{name: "missing closing boundary after a complete part", body: "Content-Type: multipart/mixed; boundary=B\r\n\r\n" + textPart("B", "hello"), wantDrop: "attachment_failed"},
		{name: "more parts than supported", body: manyParts(maxMIMEParts + 1), wantDrop: "attachment_failed"},
		{name: "nesting deeper than supported", body: nestedMultipart(maxMIMEDepth + 2), wantDrop: "attachment_failed"},
		{name: "parts exactly at the cap", body: manyParts(maxMIMEParts - 1)},
		{name: "nesting exactly at the cap", body: nestedMultipart(maxMIMEDepth)},
		{name: "plain text with no attachments", body: "Content-Type: text/plain; charset=utf-8\r\n\r\nhello\r\n"},
		{name: "message without a content type", body: "\r\nhello\r\n"},
		{name: "multipart alternative", body: "Content-Type: multipart/alternative; boundary=B\r\n\r\n" + textPart("B", "hello") + "--B\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>hello</p>\r\n--B--\r\n"},
		{name: "unknown charset is tolerated", body: "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain; charset=x-no-such-charset\r\n\r\nhello\r\n" + imagePart("B") + "--B--\r\n"},
		{name: "unknown transfer encoding is tolerated", body: "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: x-no-such-encoding\r\n\r\nhello\r\n" + imagePart("B") + "--B--\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingMedia{}
			h := newHarness(t, harnessOptions{media: store})
			h.start()
			h.deliver(withMIMEBody(mailSpec{from: friendAddress, subject: "Docs", messageID: "mime@example.com"}, tc.body))
			if tc.wantDrop != "" {
				h.wantTurns(0)
				h.wantDrops(tc.wantDrop)
				if len(store.stored()) != 0 {
					t.Errorf("stored %d artifacts for a rejected message", len(store.stored()))
				}
				return
			}
			h.wantTurns(1)
			h.wantDrops()
		})
	}
}

func TestAttachments_TolerantCasesStillStoreTheirAttachments(t *testing.T) {
	cases := map[string]string{
		"unknown charset":           "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain; charset=x-no-such-charset\r\n\r\nhello\r\n" + imagePart("B") + "--B--\r\n",
		"unknown transfer encoding": "Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: x-no-such-encoding\r\n\r\nhello\r\n" + imagePart("B") + "--B--\r\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			store := &recordingMedia{}
			h := newHarness(t, harnessOptions{media: store})
			h.start()
			h.deliver(withMIMEBody(mailSpec{from: friendAddress, subject: "Docs", messageID: "tol@example.com"}, body))
			h.wantTurns(1)
			if items := store.stored(); len(items) != 1 || items[0].meta.Filename != "p.png" {
				t.Errorf("stored = %+v, want the image attachment", items)
			}
		})
	}
}

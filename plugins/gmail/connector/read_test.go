package connector

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/modules/plugin/wire"
)

func readAPI(payload map[string]any) *fakeAPI {
	return newAPI(func(*wire.Request) (*wire.Response, error) {
		return jsonResponse(200, messageJSON("m1", payload)), nil
	})
}

func readOutputOf(t *testing.T, api *fakeAPI) readOutput {
	t.Helper()
	raw, err := invoke(t, api, "read", map[string]any{"id": "m1"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var decoded readOutput
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return decoded
}

func multipart(mimeType string, parts ...map[string]any) map[string]any {
	return map[string]any{"mimeType": mimeType, "parts": parts}
}

func TestReadReturnsHeadersBodyAndAttachments(t *testing.T) {
	payload := multipart("multipart/mixed",
		multipart("multipart/alternative", partOf("text/plain", "Plain body"), partOf("text/html", "<p>Html body</p>")),
		map[string]any{"mimeType": "application/pdf", "filename": "invoice.pdf", "body": map[string]any{"attachmentId": "att1", "size": 12345}},
	)
	payload["headers"] = headersOf("From", "Alice <alice@example.com>", "To", "me@example.com", "Cc", "bob@example.com", "Subject", "=?UTF-8?B?Q2Fmw6k=?=", "Date", "Tue, 3 Nov 2026 10:00:00 +0000", "Message-ID", "<m@example.com>")
	api := readAPI(payload)
	got := readOutputOf(t, api)
	if got.ID != "m1" || got.ThreadID != "tm1" || got.Body != "Plain body" || got.Truncated {
		t.Fatalf("output = %+v", got)
	}
	want := readHeaders{From: "Alice <alice@example.com>", To: "me@example.com", Cc: "bob@example.com", Subject: "Café", Date: "Tue, 3 Nov 2026 10:00:00 +0000", MessageID: "<m@example.com>"}
	if got.Headers != want {
		t.Fatalf("headers = %+v, want %+v", got.Headers, want)
	}
	if len(got.Attachments) != 1 || got.Attachments[0] != (attachment{Name: "invoice.pdf", MimeType: "application/pdf", Size: 12345}) {
		t.Fatalf("attachments = %+v", got.Attachments)
	}
	requests := api.seen()
	if len(requests) != 1 || requests[0].Method != http.MethodGet || queryOf(t, requests[0].URL).Get("format") != "full" || !strings.HasSuffix(strings.SplitN(requests[0].URL, "?", 2)[0], "/users/me/messages/m1") {
		t.Fatalf("requests = %+v", requests)
	}
}

func TestReadRejectsBadIDsBeforeAnyRequest(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"path traversal", "../../drafts"},
		{"slash", "a/b"},
		{"query", "m1?format=raw"},
		{"fragment", "m1#x"},
		{"percent", "m%2f1"},
		{"space", "m 1"},
		{"newline", "m1\n"},
		{"empty", ""},
		{"too long", strings.Repeat("a", 65)},
		{"non-ascii", "mé"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := readAPI(multipart("multipart/mixed"))
			_, err := invoke(t, api, "read", map[string]any{"id": tc.id})
			mustCode(t, err, CodeInvalidArgument)
			if len(api.seen()) != 0 {
				t.Fatalf("requests = %+v", api.seen())
			}
		})
	}
}

func TestReadChoosesTheBodyAndMarksWhatIsMissing(t *testing.T) {
	cases := []struct {
		name      string
		payload   map[string]any
		body      string
		truncated bool
	}{
		{"plain preferred over html", multipart("multipart/alternative", partOf("text/plain", "plain"), partOf("text/html", "<b>html</b>")), "plain", false},
		{"html only is reduced", partOf("text/html", "<p>Hello <b>world</b></p><p>Bye</p>"), "Hello world\nBye", false},
		{"single part without a mime type", partOf("", "bare"), "bare", false},
		{"several plain parts are joined", multipart("multipart/mixed", partOf("text/plain", "one"), partOf("text/plain", "two")), "one\n\ntwo", false},
		{"text part stored as an attachment is marked", multipart("multipart/mixed", partOf("text/plain", "first"), map[string]any{"mimeType": "text/plain", "body": map[string]any{"attachmentId": "big", "size": 5000000}}), "first", true},
		{"invalid base64 is marked", map[string]any{"mimeType": "text/plain", "body": map[string]any{"data": "!!!not base64!!!"}}, "", true},
		{"no payload text", multipart("multipart/mixed"), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := readOutputOf(t, readAPI(tc.payload))
			if got.Body != tc.body || got.Truncated != tc.truncated {
				t.Fatalf("body = %q truncated = %v, want %q %v", got.Body, got.Truncated, tc.body, tc.truncated)
			}
		})
	}
}

func TestReadBoundsMIMEDepthAndPartCount(t *testing.T) {
	deep := partOf("text/plain", "buried")
	for range maxMIMEDepth + 2 {
		deep = multipart("multipart/mixed", deep)
	}
	got := readOutputOf(t, readAPI(multipart("multipart/mixed", partOf("text/plain", "top"), deep)))
	if got.Body != "top" || !got.Truncated {
		t.Fatalf("deep output = %q truncated = %v", got.Body, got.Truncated)
	}
	var wide []map[string]any
	for range maxMIMEParts + 10 {
		wide = append(wide, partOf("text/plain", "x"))
	}
	got = readOutputOf(t, readAPI(multipart("multipart/mixed", wide...)))
	if !got.Truncated || strings.Count(got.Body, "x") > maxMIMEParts {
		t.Fatalf("wide output truncated = %v with %d parts", got.Truncated, strings.Count(got.Body, "x"))
	}
}

func TestReadTruncatesOnARuneBoundary(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		glyph  string
	}{
		{"three byte rune straddles the limit", "a", "日"},
		{"three byte rune ends on the limit", "", "日"},
		{"four byte rune straddles the limit", "ab", "🙂"},
		{"two byte rune straddles the limit", "a", "é"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.prefix + strings.Repeat(tc.glyph, maxReadBody)
			got := readOutputOf(t, readAPI(partOf("text/plain", text)))
			if !got.Truncated || len(got.Body) > maxReadBody || !utf8.ValidString(got.Body) || strings.ContainsRune(got.Body, utf8.RuneError) {
				t.Fatalf("truncated = %v, len = %d, valid = %v", got.Truncated, len(got.Body), utf8.ValidString(got.Body))
			}
			if maxReadBody-len(got.Body) >= utf8.UTFMax {
				t.Fatalf("cut at %d bytes, more than one rune short of %d", len(got.Body), maxReadBody)
			}
		})
	}
}

func TestReadKeepsABodyThatFitsExactly(t *testing.T) {
	got := readOutputOf(t, readAPI(partOf("text/plain", strings.Repeat("a", maxReadBody))))
	if got.Truncated || len(got.Body) != maxReadBody {
		t.Fatalf("truncated = %v, len = %d", got.Truncated, len(got.Body))
	}
}

func TestReadStripsControlAndInvisibleCharacters(t *testing.T) {
	text := "visible\x00 \x1b[31mred\x07 zero" + string(rune(0x200b)) + "width bidi" + string(rune(0x202e)) + "evil tag" + string(rune(0xE0041)) + "ged bom" + string(rune(0xFEFF)) + " c1" + string(rune(0x85)) + " line" + string(rune(0x2028)) + "sep del\x7f end"
	got := readOutputOf(t, readAPI(partOf("text/plain", text)))
	for _, r := range got.Body {
		if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
			t.Fatalf("body keeps %U: %q", r, got.Body)
		}
	}
	if !strings.Contains(got.Body, "visible") || !strings.Contains(got.Body, "end") {
		t.Fatalf("body = %q", got.Body)
	}
}

func TestReadConvertsDeclaredCharsets(t *testing.T) {
	latin := map[string]any{"mimeType": "text/plain", "headers": headersOf("Content-Type", "text/plain; charset=ISO-8859-1"), "body": map[string]any{"data": b64url("caf\xe9")}}
	got := readOutputOf(t, readAPI(latin))
	if got.Body != "café" {
		t.Fatalf("body = %q", got.Body)
	}
	unknown := map[string]any{"mimeType": "text/plain", "headers": headersOf("Content-Type", "text/plain; charset=x-nonsense"), "body": map[string]any{"data": b64url("ok\xff")}}
	got = readOutputOf(t, readAPI(unknown))
	if !strings.HasPrefix(got.Body, "ok") || !utf8.ValidString(got.Body) {
		t.Fatalf("body = %q", got.Body)
	}
}

func TestReadDropsHiddenHTMLText(t *testing.T) {
	cases := []struct {
		name   string
		markup string
		want   string
	}{
		{"script", `<p>shown</p><script>SECRET()</script>`, "shown"},
		{"style", `<style>.a{color:red}</style><p>shown</p>`, "shown"},
		{"comment", `<p>shown</p><!-- SECRET -->`, "shown"},
		{"conditional comment", `<p>shown</p><!--[if mso]>SECRET<![endif]-->`, "shown"},
		{"display none", `<p>shown</p><div style="display:none">SECRET</div>`, "shown"},
		{"display none with spacing and case", `<p>shown</p><div style="COLOR:red; Display : NONE !important">SECRET</div>`, "shown"},
		{"visibility hidden", `<p>shown</p><span style="visibility:hidden">SECRET</span>`, "shown"},
		{"hidden attribute", `<p>shown</p><p hidden>SECRET</p>`, "shown"},
		{"zero font size", `<p>shown</p><span style="font-size:0px">SECRET</span>`, "shown"},
		{"zero opacity", `<p>shown</p><span style="opacity:0">SECRET</span>`, "shown"},
		{"mso hide", `<p>shown</p><span style="mso-hide:all">SECRET</span>`, "shown"},
		{"aria hidden", `<p>shown</p><div aria-hidden="true">SECRET</div>`, "shown"},
		{"aria hidden with case and spacing", `<p>shown</p><div aria-hidden=" TRUE ">SECRET</div>`, "shown"},
		{"textarea", `<p>shown</p><textarea>SECRET</textarea>`, "shown"},
		{"zero percent opacity", `<p>shown</p><span style="opacity:0%">SECRET</span>`, "shown"},
		{"self closing hidden tag stays open", `<div hidden/>SECRET</div><p>shown</p>`, "shown"},
		{"self closing styled tag stays open", `<span style="display:none"/>SECRET</span><p>shown</p>`, "shown"},
		{"self closing script stays open", `<script/>SECRET()</script><p>shown</p>`, "shown"},
		{"self closing svg closes", `<svg/><p>shown</p>`, "shown"},
		{"aria hidden false stays visible", `<p aria-hidden="false">shown</p>`, "shown"},
		{"nested inside a hidden block", `<div style="display:none"><div>one</div><p>SECRET<div>two</div></p>SECRET</div><p>shown</p>`, "shown"},
		{"hidden input", `<p>shown</p><input type="hidden" value="SECRET">`, "shown"},
		{"title and head", `<html><head><title>SECRET</title></head><body><p>shown</p></body></html>`, "shown"},
		{"head never closed", `<html><head><title>SECRET</title><body><p>shown</p></body></html>`, "shown"},
		{"noscript", `<noscript>SECRET</noscript><p>shown</p>`, "shown"},
		{"template", `<template>SECRET</template><p>shown</p>`, "shown"},
		{"visible text after a closed hidden block", `<span style="display:none">SECRET</span>after`, "after"},
		{"entities decode", `<p>a &amp; b &lt;c&gt;</p>`, "a & b <c>"},
		{"inline style that is visible", `<p style="display:block;font-size:12px;opacity:1">shown</p>`, "shown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := readOutputOf(t, readAPI(partOf("text/html", tc.markup)))
			if strings.Contains(got.Body, "SECRET") || !strings.Contains(got.Body, tc.want) {
				t.Fatalf("body = %q, want %q without SECRET", got.Body, tc.want)
			}
		})
	}
}

func TestReadBoundsDeeplyNestedHTML(t *testing.T) {
	markup := strings.Repeat(`<div style="display:none">`, maxHTMLDepth+50) + "SECRET"
	got := readOutputOf(t, readAPI(partOf("text/html", "<p>shown</p>"+markup)))
	if strings.Contains(got.Body, "SECRET") || !strings.Contains(got.Body, "shown") || !got.Truncated {
		t.Fatalf("body = %q truncated = %v", got.Body, got.Truncated)
	}
}

func TestReadSanitizesAttachmentNamesAndHeaders(t *testing.T) {
	payload := multipart("multipart/mixed", partOf("text/plain", "body"),
		map[string]any{"mimeType": "text/plain", "filename": "a\r\nb\x00\u202e.txt", "body": map[string]any{"size": -5}},
	)
	payload["headers"] = headersOf("Subject", "Hi\r\nBcc: x\u202e", "From", strings.Repeat("a", 5000))
	got := readOutputOf(t, readAPI(payload))
	if len(got.Attachments) != 1 || got.Attachments[0].Name != "a b.txt" || got.Attachments[0].Size != 0 {
		t.Fatalf("attachments = %+v", got.Attachments)
	}
	if got.Headers.Subject != "Hi Bcc: x" || utf8.RuneCountInString(got.Headers.From) != maxHeaderRunes {
		t.Fatalf("headers = %+v", got.Headers)
	}
}

func TestReadCapsTheNumberOfAttachments(t *testing.T) {
	var parts []map[string]any
	for range maxAttachments + 5 {
		parts = append(parts, map[string]any{"mimeType": "application/zip", "filename": "f.zip", "body": map[string]any{"size": 1}})
	}
	got := readOutputOf(t, readAPI(multipart("multipart/mixed", parts...)))
	if len(got.Attachments) != maxAttachments || !got.Truncated {
		t.Fatalf("attachments = %d truncated = %v", len(got.Attachments), got.Truncated)
	}
}

func TestReadOfAnOversizedMailReturnsTheErrorAndKeepsServing(t *testing.T) {
	calls := 0
	api := newAPI(func(*wire.Request) (*wire.Response, error) {
		calls++
		if calls == 1 {
			return &wire.Response{Failure: "response_too_large"}, nil
		}
		return jsonResponse(200, messageJSON("m2", partOf("text/plain", "small"))), nil
	})
	_, err := invoke(t, api, "read", map[string]any{"id": "m1"})
	mustCode(t, err, CodeMessageTooLarge)
	raw, err := invoke(t, api, "read", map[string]any{"id": "m2"})
	if err != nil || !strings.Contains(string(raw), "small") {
		t.Fatalf("second read = %s, %v", raw, err)
	}
}

func TestReadRefusesResponsesThatAreNotMessages(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"not json", []byte("<html>proxy error</html>")},
		{"empty", nil},
		{"another id", mustJSON(messageJSON("other", partOf("text/plain", "x")))},
		{"thread id with a path", mustJSON(map[string]any{"id": "m1", "threadId": "../x", "payload": partOf("text/plain", "x")})},
		{"no payload", mustJSON(map[string]any{"id": "m1", "threadId": "t1"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newAPI(func(*wire.Request) (*wire.Response, error) { return &wire.Response{Status: 200, Body: tc.body}, nil })
			_, err := invoke(t, api, "read", map[string]any{"id": "m1"})
			mustCode(t, err, CodeResponseInvalid)
		})
	}
}

func TestReadMapsGmailStatusesToStaticCodes(t *testing.T) {
	for status, want := range map[int]Code{401: CodeUnauthorized, 403: CodeForbidden, 404: CodeNotFound, 429: CodeRateLimited, 500: CodeUpstream, 503: CodeUpstream} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			api := newAPI(func(*wire.Request) (*wire.Response, error) { return statusOnly(status), nil })
			_, err := invoke(t, api, "read", map[string]any{"id": "m1"})
			mustCode(t, err, want)
		})
	}
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

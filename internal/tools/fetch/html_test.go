package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

func reduce(t *testing.T, source string, maxBytes int64) (string, bool) {
	t.Helper()
	rendered, truncated, err := HTMLToMarkdown(context.Background(), []byte(source), maxBytes)
	if err != nil {
		t.Fatalf("HTMLToMarkdown: %v", err)
	}
	return rendered, truncated
}

func TestHTMLToMarkdownDropsInvisibleContent(t *testing.T) {
	cases := []struct {
		name   string
		source string
		secret string
	}{
		{"comment", `<p>visible</p><!-- secret --><p>after</p>`, "secret"},
		{"script", `<p>visible</p><script>secret()</script>`, "secret"},
		{"style", `<p>visible</p><style>.secret{}</style>`, "secret"},
		{"noscript", `<p>visible</p><noscript>secret</noscript>`, "secret"},
		{"input", `<p>visible</p><input value="secret" placeholder="secret">`, "secret"},
		{"textarea", `<p>visible</p><textarea>secret</textarea>`, "secret"},
		{"hidden attribute", `<p>visible</p><div hidden>secret</div>`, "secret"},
		{"hidden attribute with value", `<p>visible</p><div hidden="until-found">secret</div>`, "secret"},
		{"aria-hidden", `<p>visible</p><div aria-hidden="true">secret</div>`, "secret"},
		{"aria-hidden mixed case", `<p>visible</p><div aria-hidden=" TRUE ">secret</div>`, "secret"},
		{"display none", `<p>visible</p><div style="color:red; display : none">secret</div>`, "secret"},
		{"display none important", `<p>visible</p><div style="DISPLAY:NONE !important">secret</div>`, "secret"},
		{"visibility hidden", `<p>visible</p><span style="visibility:hidden">secret</span>`, "secret"},
		{"opacity zero", `<p>visible</p><span style="opacity:0">secret</span>`, "secret"},
		{"opacity fractional zero", `<p>visible</p><span style="opacity: 0.0">secret</span>`, "secret"},
		{"font size zero", `<p>visible</p><span style="font-size:0">secret</span>`, "secret"},
		{"font size zero pixels", `<p>visible</p><span style="font-size: 0px">secret</span>`, "secret"},
		{"hidden descendant", `<p>visible</p><div hidden><p><b>secret</b></p></div>`, "secret"},
		{"script inside pre", `<p>visible</p><pre>code<script>secret()</script></pre>`, "secret"},
		{"hidden inside pre", `<p>visible</p><pre>code<span hidden>secret</span></pre>`, "secret"},
		{"comment inside pre", `<p>visible</p><pre>code<!-- secret --></pre>`, "secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rendered, _ := reduce(t, tc.source, 1<<20)
			if strings.Contains(rendered, tc.secret) {
				t.Fatalf("rendered output leaks hidden content: %q", rendered)
			}
			if !strings.Contains(rendered, "visible") {
				t.Fatalf("rendered output lost visible content: %q", rendered)
			}
		})
	}
}

func TestHTMLToMarkdownKeepsVisibleStyledContent(t *testing.T) {
	for _, source := range []string{
		`<span style="opacity:0.5">shown</span>`,
		`<span style="opacity:1">shown</span>`,
		`<span style="font-size:0.8em">shown</span>`,
		`<span style="font-size:10px">shown</span>`,
		`<span style="display:block;visibility:visible">shown</span>`,
		`<span aria-hidden="false">shown</span>`,
		`<span style="color:red">shown</span>`,
	} {
		t.Run(source, func(t *testing.T) {
			if rendered, _ := reduce(t, source, 1<<20); rendered != "shown" {
				t.Fatalf("rendered = %q, want shown", rendered)
			}
		})
	}
}

func TestHTMLToMarkdownFailsClosedOnOverDeepDocument(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{"nested divs", strings.Repeat("<div>", 600) + "secret<!-- secret --><script>secret()</script>"},
		{"nested divs closed", strings.Repeat("<div>", 600) + "secret" + strings.Repeat("</div>", 600)},
		{"nested inline", strings.Repeat("<span>", 5000) + "secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rendered, truncated, err := HTMLToMarkdown(context.Background(), []byte(tc.source), 1<<20)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeHTMLParseFailed {
				t.Fatalf("err = %v, want %s", err, ErrorCodeHTMLParseFailed)
			}
			if rendered != "" || truncated {
				t.Fatalf("failed reduction returned content: %q truncated=%v", rendered, truncated)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaks document content: %v", err)
			}
		})
	}
}

func TestHTMLToMarkdownAcceptsDocumentJustUnderDepthLimit(t *testing.T) {
	rendered, _ := reduce(t, strings.Repeat("<div>", 300)+"deep", 1<<20)
	if rendered != "deep" {
		t.Fatalf("rendered = %q, want deep", rendered)
	}
}

func TestHTMLToMarkdownTruncatesOnRuneBoundary(t *testing.T) {
	const source = `<p>héllo wörld 日本語のテキスト 🙂🙂🙂 end</p>`
	full, _ := reduce(t, source, 1<<20)
	for limit := int64(1); limit < int64(len(full)); limit++ {
		rendered, truncated := reduce(t, source, limit)
		if !truncated {
			t.Fatalf("limit %d: truncated = false", limit)
		}
		if !utf8.ValidString(rendered) {
			t.Fatalf("limit %d: invalid UTF-8 %q", limit, rendered)
		}
		if int64(len(rendered)) > limit || !strings.HasPrefix(full, rendered) {
			t.Fatalf("limit %d: rendered %q is not a bounded prefix of %q", limit, rendered, full)
		}
	}
	if rendered, truncated := reduce(t, source, int64(len(full))); truncated || rendered != full {
		t.Fatalf("limit at length: rendered %q truncated=%v", rendered, truncated)
	}
}

func TestHTMLToMarkdownReplacesInvalidUTF8(t *testing.T) {
	rendered, _ := reduce(t, "<p>a\xff\xfeb</p>", 1<<20)
	if !utf8.ValidString(rendered) {
		t.Fatalf("rendered output is not valid UTF-8: %q", rendered)
	}
	rendered, truncated := reduce(t, "<p>"+strings.Repeat("\xffa", 40)+"</p>", 10)
	if !truncated || !utf8.ValidString(rendered) || len(rendered) > 10 {
		t.Fatalf("rendered = %q truncated=%v", rendered, truncated)
	}
}

func TestHTMLToMarkdownStopsOnCancelledContext(t *testing.T) {
	source := []byte(strings.Repeat(`<a href="https://example.com/x">link</a>`, 20000))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rendered, truncated, err := HTMLToMarkdown(ctx, source, 1<<20)
	if !errors.Is(err, context.Canceled) || rendered != "" || truncated {
		t.Fatalf("HTMLToMarkdown = %q, %v, %v; want context.Canceled and no content", rendered, truncated, err)
	}
	expired, expire := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expire()
	if _, _, err := HTMLToMarkdown(expired, source, 1<<20); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestMarkdownRendererStopsMidRenderOnCancellation(t *testing.T) {
	const links = 20000
	document, err := html.Parse(strings.NewReader(strings.Repeat(`<a href="https://example.com/x">link</a>`, links)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	full := &markdownRenderer{}
	full.renderChildren(context.Background(), document)
	if full.err != nil {
		t.Fatalf("uncancelled render failed: %v", full.err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped := &markdownRenderer{}
	stopped.renderChildren(ctx, document)
	if !errors.Is(stopped.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", stopped.err)
	}
	if len(stopped.out) >= len(full.out)/10 {
		t.Fatalf("cancelled render produced %d of %d bytes", len(stopped.out), len(full.out))
	}
}

func TestHTMLToMarkdownRendersInLinearTime(t *testing.T) {
	const small, large = 64 << 10, 1 << 20
	const factor = large / small
	const maxRatio = 3 * factor
	shapes := []struct {
		name string
		unit string
	}{
		{"anchors", `<a href="https://example.com/x">link</a>`},
		{"strong", `<b>bold</b> `},
		{"code", `<code>x</code> `},
		{"list items", `<ul><li>item</li></ul>`},
	}
	measure := func(source []byte) time.Duration {
		best := time.Duration(1<<63 - 1)
		for range 3 {
			start := time.Now()
			if _, _, err := HTMLToMarkdown(context.Background(), source, 1<<30); err != nil {
				t.Fatalf("HTMLToMarkdown: %v", err)
			}
			if elapsed := time.Since(start); elapsed < best {
				best = elapsed
			}
		}
		return best
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			smallTime := measure([]byte(strings.Repeat(shape.unit, small/len(shape.unit))))
			largeTime := measure([]byte(strings.Repeat(shape.unit, large/len(shape.unit))))
			if smallTime <= 0 {
				smallTime = time.Microsecond
			}
			if ratio := float64(largeTime) / float64(smallTime); ratio > maxRatio {
				t.Fatalf("%dx input took %.1fx longer (%v -> %v), want at most %dx", factor, ratio, smallTime, largeTime, maxRatio)
			}
		})
	}
}

func TestFetchFailsClosedOnOverDeepHTML(t *testing.T) {
	document := strings.Repeat("<div>", 600) + "secret<!-- secret --><script>secret()</script>"
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxRedirects:    2,
		MaxEncodedBytes: 8192,
		MaxDecodedBytes: 8192,
		client:          chunkedClient("text/html", document),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	outcome, err := adapter(context.Background(), fetchRequest("https://public.example/deep"), constraints())
	if code, ok := CodeOf(err); !ok || code != ErrorCodeHTMLParseFailed {
		t.Fatalf("err = %v, want %s", err, ErrorCodeHTMLParseFailed)
	}
	if len(outcome.Output) != 0 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("failed reduction leaked content: output=%q err=%v", outcome.Output, err)
	}
}

func TestFetchTruncatedHTMLBodyIsValidUTF8(t *testing.T) {
	adapter, err := New(Options{
		Timeout:         time.Second,
		MaxRedirects:    2,
		MaxEncodedBytes: 8192,
		MaxDecodedBytes: 11,
		client:          chunkedClient("text/html", "<p>"+strings.Repeat("日本語", 3)+"</p>"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	outcome, err := adapter(context.Background(), fetchRequest("https://public.example/doc"), constraints())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var body result
	if err := json.Unmarshal(outcome.Output, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Truncated || !utf8.ValidString(body.Body) {
		t.Fatalf("body = %q truncated=%v, want valid UTF-8 truncation", body.Body, body.Truncated)
	}
}

func FuzzHTMLToMarkdown(f *testing.F) {
	seeds := []string{
		`<p>hello <b>world</b></p>`,
		`<a href="https://example.com">x</a><script>alert(1)</script><!-- c -->`,
		`<div hidden>secret</div><span style="display:none">x</span><pre>a<b>b</b></pre>`,
		strings.Repeat("<div>", 600),
		"<p>日本語 \xff\xfe 🙂</p>",
		`<ul><li><code>x</code></li></ul><textarea>t</textarea><input value=v>`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed), uint16(64))
	}
	f.Fuzz(func(t *testing.T, source []byte, limit uint16) {
		maxBytes := int64(limit) + 1
		rendered, truncated, err := HTMLToMarkdown(context.Background(), source, maxBytes)
		if err != nil {
			if rendered != "" || truncated {
				t.Fatalf("failed reduction returned content: %q", rendered)
			}
			return
		}
		if !utf8.ValidString(rendered) || int64(len(rendered)) > maxBytes {
			t.Fatalf("rendered %d bytes (limit %d), valid=%v", len(rendered), maxBytes, utf8.ValidString(rendered))
		}
	})
}

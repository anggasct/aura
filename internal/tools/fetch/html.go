package fetch

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

func isHTML(contentType string) bool {
	return contentType == "text/html" || contentType == "application/xhtml+xml"
}

// htmlToMarkdown renders a bounded HTML document as clean markdown for
// model consumption. Non-visible content such as scripts and styles is
// dropped; the rendered output is capped at maxBytes.
func htmlToMarkdown(source []byte, maxBytes int64) (string, bool) {
	node, err := html.Parse(bytes.NewReader(source))
	if err != nil {
		return string(source), false
	}
	renderer := &markdownRenderer{}
	renderer.renderChildren(node)
	rendered := strings.TrimSpace(compactBlankLines(renderer.builder.String()))
	truncated := int64(len(rendered)) > maxBytes
	if truncated {
		rendered = rendered[:maxBytes]
	}
	return rendered, truncated
}

// markdownRenderer tracks whether the last write closed an inline element
// so word separation survives tag stripping.
type markdownRenderer struct {
	builder          strings.Builder
	afterInlineClose bool
}

func (r *markdownRenderer) renderChildren(node *html.Node) {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		r.renderNode(child)
	}
}

func (r *markdownRenderer) renderNode(node *html.Node) {
	switch node.Type {
	case html.CommentNode, html.DoctypeNode:
		return
	case html.TextNode:
		r.text(node.Data)
		return
	case html.ElementNode:
	default:
		r.renderChildren(node)
		return
	}
	switch node.Data {
	case "script", "style", "noscript", "template", "head", "title", "meta", "link", "iframe", "svg", "canvas", "object", "embed":
		return
	case "h1", "h2", "h3", "h4", "h5", "h6":
		r.ensureBlock()
		r.builder.WriteString(strings.Repeat("#", int(node.Data[1]-'0')) + " ")
		r.renderChildren(node)
		r.ensureBlock()
	case "p", "div", "section", "article", "header", "footer", "nav", "aside", "main", "figure", "figcaption", "dd", "dt", "summary", "form", "fieldset":
		r.ensureBlock()
		r.renderChildren(node)
		r.ensureBlock()
	case "br":
		r.builder.WriteString("\n")
	case "hr":
		r.ensureBlock()
		r.builder.WriteString("---")
		r.ensureBlock()
	case "ul", "ol", "dl":
		r.ensureBlock()
		r.renderChildren(node)
		r.ensureBlock()
	case "li":
		r.ensureLine()
		r.builder.WriteString("- ")
		r.renderChildren(node)
		r.builder.WriteString("\n")
	case "blockquote":
		r.ensureBlock()
		r.builder.WriteString("> ")
		r.renderChildren(node)
		r.ensureBlock()
	case "pre":
		r.ensureBlock()
		r.builder.WriteString("```\n")
		r.writeRawText(node)
		r.builder.WriteString("\n```")
		r.ensureBlock()
	case "code":
		r.openInline("`")
		r.renderChildren(node)
		r.closeInline("`")
	case "strong", "b":
		r.openInline("**")
		r.renderChildren(node)
		r.closeInline("**")
	case "em", "i":
		r.openInline("*")
		r.renderChildren(node)
		r.closeInline("*")
	case "a":
		r.openInline("[")
		r.renderChildren(node)
		r.closeInline("]")
		if href := attribute(node, "href"); href != "" {
			r.builder.WriteString("(" + href + ")")
			r.afterInlineClose = true
		}
	default:
		r.renderChildren(node)
	}
}

// text collapses whitespace runs so source formatting cannot leak into the
// rendered markdown, inserting a single separating space after inline
// closes; punctuation stays attached to the preceding word.
func (r *markdownRenderer) text(text string) {
	collapsed := strings.Join(strings.Fields(text), " ")
	if collapsed == "" {
		if text != "" && r.afterInlineClose && !r.endsWithSpace() {
			r.builder.WriteString(" ")
			r.afterInlineClose = false
		}
		return
	}
	if r.afterInlineClose && !r.endsWithSpace() && startsWithWord(collapsed) {
		r.builder.WriteString(" ")
	}
	r.afterInlineClose = false
	r.builder.WriteString(collapsed)
	r.builder.WriteString(" ")
}

func (r *markdownRenderer) openInline(marker string) {
	r.afterInlineClose = false
	r.builder.WriteString(marker)
}

func (r *markdownRenderer) closeInline(marker string) {
	trimmed := strings.TrimRight(r.builder.String(), " ")
	r.builder.Reset()
	r.builder.WriteString(trimmed)
	r.builder.WriteString(marker)
	r.afterInlineClose = true
}

// writeRawText preserves whitespace inside preformatted blocks.
func (r *markdownRenderer) writeRawText(node *html.Node) {
	if node.Type == html.TextNode {
		r.builder.WriteString(node.Data)
		return
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		r.writeRawText(child)
	}
}

func (r *markdownRenderer) body() string { return r.builder.String() }

func (r *markdownRenderer) endsWithSpace() bool {
	return strings.HasSuffix(r.body(), " ") || strings.HasSuffix(r.body(), "\n")
}

func (r *markdownRenderer) ensureBlock() {
	if r.builder.Len() == 0 {
		return
	}
	r.afterInlineClose = false
	body := r.body()
	if strings.HasSuffix(body, "\n\n") {
		return
	}
	if strings.HasSuffix(body, "\n") {
		r.builder.WriteString("\n")
		return
	}
	r.builder.WriteString("\n\n")
}

func (r *markdownRenderer) ensureLine() {
	r.afterInlineClose = false
	if r.builder.Len() == 0 || strings.HasSuffix(r.body(), "\n") {
		return
	}
	r.builder.WriteString("\n")
}

func startsWithWord(text string) bool {
	first, _ := utf8.DecodeRuneInString(text)
	return unicode.IsLetter(first) || unicode.IsDigit(first)
}

func attribute(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func compactBlankLines(text string) string {
	lines := strings.Split(text, "\n")
	collapsed := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			blank = true
			continue
		}
		if blank && len(collapsed) > 0 {
			collapsed = append(collapsed, "")
		}
		blank = false
		collapsed = append(collapsed, strings.TrimRight(line, " \t"))
	}
	return strings.Join(collapsed, "\n")
}

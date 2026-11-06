package fetch

import (
	"bytes"
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

func isHTML(contentType string) bool {
	return contentType == "text/html" || contentType == "application/xhtml+xml"
}

const cancelCheckInterval = 256

func htmlToMarkdown(ctx context.Context, source []byte, maxBytes int64) (markdown string, truncated bool, err error) {
	if cancelled := ctx.Err(); cancelled != nil {
		return "", false, cancelled
	}
	reader := bytes.NewReader(source)
	node, parseErr := html.Parse(readerFunc(func(buffer []byte) (int, error) {
		if cancelled := ctx.Err(); cancelled != nil {
			return 0, cancelled
		}
		return reader.Read(buffer)
	}))
	if cancelled := ctx.Err(); cancelled != nil {
		return "", false, cancelled
	}
	if parseErr != nil {
		return "", false, errorf(ErrorCodeHTMLParseFailed, "html document could not be parsed")
	}
	renderer := &markdownRenderer{}
	renderer.renderChildren(ctx, node)
	if renderer.err != nil {
		return "", false, renderer.err
	}
	markdown = strings.ToValidUTF8(strings.TrimSpace(compactBlankLines(string(renderer.out))), "\uFFFD")
	if cancelled := ctx.Err(); cancelled != nil {
		return "", false, cancelled
	}
	if int64(len(markdown)) > maxBytes {
		return markdown[:runeBoundary(markdown, maxBytes)], true, nil
	}
	return markdown, false, nil
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(buffer []byte) (int, error) { return f(buffer) }

func runeBoundary(text string, limit int64) int {
	cut := int(limit)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return cut
}

type markdownRenderer struct {
	out              []byte
	afterInlineClose bool
	visited          int
	err              error
}

func (r *markdownRenderer) renderChildren(ctx context.Context, node *html.Node) {
	for child := node.FirstChild; child != nil && r.err == nil; child = child.NextSibling {
		r.renderNode(ctx, child)
	}
}

func (r *markdownRenderer) renderNode(ctx context.Context, node *html.Node) {
	r.visited++
	if r.visited%cancelCheckInterval == 0 {
		if err := ctx.Err(); err != nil {
			r.err = err
			return
		}
	}
	switch node.Type {
	case html.CommentNode, html.DoctypeNode:
		return
	case html.TextNode:
		r.text(node.Data)
		return
	case html.ElementNode:
	default:
		r.renderChildren(ctx, node)
		return
	}
	if droppedElement(node) {
		return
	}
	switch node.Data {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		r.ensureBlock()
		r.write(strings.Repeat("#", int(node.Data[1]-'0')) + " ")
		r.renderChildren(ctx, node)
		r.ensureBlock()
	case "p", "div", "section", "article", "header", "footer", "nav", "aside", "main", "figure", "figcaption", "dd", "dt", "summary", "form", "fieldset":
		r.ensureBlock()
		r.renderChildren(ctx, node)
		r.ensureBlock()
	case "br":
		r.write("\n")
	case "hr":
		r.ensureBlock()
		r.write("---")
		r.ensureBlock()
	case "ul", "ol", "dl":
		r.ensureBlock()
		r.renderChildren(ctx, node)
		r.ensureBlock()
	case "li":
		r.ensureLine()
		r.write("- ")
		r.renderChildren(ctx, node)
		r.write("\n")
	case "blockquote":
		r.ensureBlock()
		r.write("> ")
		r.renderChildren(ctx, node)
		r.ensureBlock()
	case "pre":
		r.ensureBlock()
		r.write("```\n")
		r.writeRawText(node)
		r.write("\n```")
		r.ensureBlock()
	case "code":
		r.openInline("`")
		r.renderChildren(ctx, node)
		r.closeInline("`")
	case "strong", "b":
		r.openInline("**")
		r.renderChildren(ctx, node)
		r.closeInline("**")
	case "em", "i":
		r.openInline("*")
		r.renderChildren(ctx, node)
		r.closeInline("*")
	case "a":
		r.openInline("[")
		r.renderChildren(ctx, node)
		r.closeInline("]")
		if href := attribute(node, "href"); href != "" {
			r.write("(" + href + ")")
			r.afterInlineClose = true
		}
	default:
		r.renderChildren(ctx, node)
	}
}

func (r *markdownRenderer) text(text string) {
	collapsed := strings.Join(strings.Fields(text), " ")
	if collapsed == "" {
		if text != "" && r.afterInlineClose && !r.endsWithSpace() {
			r.write(" ")
			r.afterInlineClose = false
		}
		return
	}
	if r.afterInlineClose && !r.endsWithSpace() && startsWithWord(collapsed) {
		r.write(" ")
	}
	r.afterInlineClose = false
	r.write(collapsed)
	r.write(" ")
}

func (r *markdownRenderer) openInline(marker string) {
	r.afterInlineClose = false
	r.write(marker)
}

func (r *markdownRenderer) closeInline(marker string) {
	r.out = bytes.TrimRight(r.out, " ")
	r.write(marker)
	r.afterInlineClose = true
}

func (r *markdownRenderer) writeRawText(node *html.Node) {
	if node.Type == html.TextNode {
		r.write(node.Data)
		return
	}
	if node.Type == html.ElementNode && droppedElement(node) {
		return
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		r.writeRawText(child)
	}
}

func (r *markdownRenderer) write(text string) { r.out = append(r.out, text...) }

func (r *markdownRenderer) endsWithSpace() bool {
	return bytes.HasSuffix(r.out, []byte(" ")) || bytes.HasSuffix(r.out, []byte("\n"))
}

func (r *markdownRenderer) ensureBlock() {
	if len(r.out) == 0 {
		return
	}
	r.afterInlineClose = false
	if bytes.HasSuffix(r.out, []byte("\n\n")) {
		return
	}
	if bytes.HasSuffix(r.out, []byte("\n")) {
		r.write("\n")
		return
	}
	r.write("\n\n")
}

func (r *markdownRenderer) ensureLine() {
	r.afterInlineClose = false
	if len(r.out) == 0 || bytes.HasSuffix(r.out, []byte("\n")) {
		return
	}
	r.write("\n")
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

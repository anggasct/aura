package telegram

import (
	"net/url"
	"regexp"
	"strings"
	"unicode/utf16"
)

const (
	parseModeHTML      = "HTML"
	fenceMarker        = "```"
	fenceCloseUnits    = len("\n" + fenceMarker)
	maxFenceLineBytes  = 128
	maxFenceIndent     = 3
	maxLanguageLength  = 32
	languageClassStart = `<pre><code class="language-`
)

var fenceLanguagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_+#.-]*$`)

type fence struct {
	language string
	bare     bool
}

func parseFence(line string) (fence, bool) {
	if len(line) > maxFenceLineBytes {
		return fence{}, false
	}
	trimmed := strings.TrimRight(line, "\r\n")
	unindented := strings.TrimLeft(trimmed, " ")
	if len(trimmed)-len(unindented) > maxFenceIndent {
		return fence{}, false
	}
	rest, found := strings.CutPrefix(unindented, fenceMarker)
	if !found {
		return fence{}, false
	}
	info := strings.TrimSpace(strings.TrimLeft(rest, "`"))
	if strings.Contains(info, "`") {
		return fence{}, false
	}
	parsed := fence{bare: info == ""}
	if fields := strings.Fields(info); len(fields) > 0 && len(fields[0]) <= maxLanguageLength && fenceLanguagePattern.MatchString(fields[0]) {
		parsed.language = fields[0]
	}
	return parsed, true
}

func renderHTML(source string) string {
	var out strings.Builder
	var code strings.Builder
	inFence := false
	language := ""
	for _, line := range strings.SplitAfter(source, "\n") {
		parsed, isFence := parseFence(line)
		switch {
		case inFence && isFence && parsed.bare:
			writeCodeBlock(&out, language, code.String())
			code.Reset()
			inFence = false
			if strings.HasSuffix(line, "\n") {
				out.WriteString("\n")
			}
		case inFence:
			code.WriteString(line)
		case isFence:
			inFence = true
			language = parsed.language
		default:
			out.WriteString(renderInline(line, true))
		}
	}
	if inFence {
		writeCodeBlock(&out, language, code.String())
	}
	return out.String()
}

func writeCodeBlock(out *strings.Builder, language, code string) {
	code = strings.TrimSuffix(code, "\n")
	if language == "" {
		out.WriteString("<pre>")
		out.WriteString(escapeText(code))
		out.WriteString("</pre>")
		return
	}
	out.WriteString(languageClassStart)
	out.WriteString(escapeAttribute(language))
	out.WriteString(`">`)
	out.WriteString(escapeText(code))
	out.WriteString("</code></pre>")
}

var emphasisTags = []struct {
	delimiter string
	tag       string
}{
	{delimiter: "**", tag: "b"},
	{delimiter: "__", tag: "b"},
	{delimiter: "~~", tag: "s"},
	{delimiter: "*", tag: "i"},
	{delimiter: "_", tag: "i"},
}

func renderInline(s string, links bool) string {
	var out strings.Builder
	i := 0
	for i < len(s) {
		if html, next, ok := inlineAt(s, i, links); ok {
			out.WriteString(html)
			i = next
			continue
		}
		if s[i] == '\\' && i+1 < len(s) && isASCIIPunctuation(s[i+1]) {
			i++
		}
		writeEscapedByte(&out, s[i])
		i++
	}
	return out.String()
}

func writeEscapedByte(out *strings.Builder, c byte) {
	switch c {
	case '&':
		out.WriteString("&amp;")
	case '<':
		out.WriteString("&lt;")
	case '>':
		out.WriteString("&gt;")
	default:
		out.WriteByte(c)
	}
}

func inlineAt(s string, i int, links bool) (html string, next int, ok bool) {
	switch s[i] {
	case '`':
		return codeSpanAt(s, i)
	case '[':
		if links {
			return linkAt(s, i)
		}
	case '*', '_', '~':
		for _, emphasis := range emphasisTags {
			if !strings.HasPrefix(s[i:], emphasis.delimiter) {
				continue
			}
			if inner, after, found := emphasisAt(s, i, emphasis.delimiter); found {
				return "<" + emphasis.tag + ">" + renderInline(inner, links) + "</" + emphasis.tag + ">", after, true
			}
		}
	}
	return "", 0, false
}

func codeSpanAt(s string, i int) (html string, next int, ok bool) {
	run := backtickRun(s, i)
	for j := i + run; j < len(s); {
		if s[j] != '`' {
			j++
			continue
		}
		closing := backtickRun(s, j)
		if closing == run && j > i+run {
			return "<code>" + escapeText(s[i+run:j]) + "</code>", j + run, true
		}
		j += closing
	}
	return "", 0, false
}

func backtickRun(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}

func emphasisAt(s string, i int, delimiter string) (inner string, next int, ok bool) {
	open := i + len(delimiter)
	if open >= len(s) || isSpaceByte(s[open]) {
		return "", 0, false
	}
	single := len(delimiter) == 1
	marker := delimiter[0]
	if single && s[open] == marker {
		return "", 0, false
	}
	if marker == '_' && i > 0 && isWordByte(s[i-1]) {
		return "", 0, false
	}
	for j := open + 1; j+len(delimiter) <= len(s); j++ {
		if s[j:j+len(delimiter)] != delimiter {
			continue
		}
		if !single {
			for j+len(delimiter) < len(s) && s[j+len(delimiter)] == marker {
				j++
			}
		}
		if isSpaceByte(s[j-1]) {
			continue
		}
		after := j + len(delimiter)
		if single && (s[j-1] == marker || (after < len(s) && s[after] == marker)) {
			continue
		}
		if marker == '_' && after < len(s) && isWordByte(s[after]) {
			continue
		}
		return s[open:j], after, true
	}
	return "", 0, false
}

func linkAt(s string, i int) (html string, next int, ok bool) {
	textEnd := strings.IndexByte(s[i+1:], ']')
	if textEnd <= 0 {
		return "", 0, false
	}
	textEnd += i + 1
	if textEnd+1 >= len(s) || s[textEnd+1] != '(' {
		return "", 0, false
	}
	targetStart := textEnd + 2
	targetEnd := strings.IndexByte(s[targetStart:], ')')
	if targetEnd < 0 {
		return "", 0, false
	}
	target := s[targetStart : targetStart+targetEnd]
	if !safeLinkTarget(target) {
		return "", 0, false
	}
	return `<a href="` + escapeAttribute(target) + `">` + renderInline(s[i+1:textEnd], false) + "</a>", targetStart + targetEnd + 1, true
}

func safeLinkTarget(target string) bool {
	if target == "" || strings.ContainsAny(target, " \t\r\n<>\"'`\\") {
		return false
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return scheme == "http" || scheme == "https"
}

var (
	textEscaper      = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	attributeEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
)

func escapeText(s string) string {
	return textEscaper.Replace(s)
}

func escapeAttribute(s string) string {
	return attributeEscaper.Replace(s)
}

func isASCIIPunctuation(c byte) bool {
	return strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", c) >= 0
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isWordByte(c byte) bool {
	return c >= 0x80 || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

type splitter struct {
	chunks  []string
	current strings.Builder
	units   int
	base    int
	hasText bool
	reopen  string
}

func splitMessage(text string) []string {
	var s splitter
	for _, line := range strings.SplitAfter(text, "\n") {
		s.writeLine(line)
	}
	s.flush(true)
	return s.chunks
}

func (s *splitter) writeLine(line string) {
	parsed, isFence := parseFence(line)
	opens := s.reopen == "" && isFence
	closes := s.reopen != "" && isFence && parsed.bare
	reserve := 0
	if opens || (s.reopen != "" && !closes) {
		reserve = fenceCloseUnits
	}
	if s.units > s.base && s.units+utf16Len(line)+reserve > messageLimit {
		s.flush(false)
	}
	for _, r := range line {
		width := runeUnits(r)
		if s.units+width+reserve > messageLimit {
			s.flush(false)
		}
		s.current.WriteRune(r)
		s.units += width
		if !isSpaceRune(r) {
			s.hasText = true
		}
	}
	switch {
	case opens:
		s.reopen = fenceMarker + parsed.language + "\n"
	case closes:
		s.reopen = ""
	}
}

func (s *splitter) flush(final bool) {
	if s.hasText {
		chunk := s.current.String()
		if s.reopen != "" && !final {
			if !strings.HasSuffix(chunk, "\n") {
				chunk += "\n"
			}
			chunk += fenceMarker
		}
		s.chunks = append(s.chunks, chunk)
	}
	s.current.Reset()
	s.units = 0
	s.hasText = false
	if s.reopen != "" {
		s.current.WriteString(s.reopen)
		s.units = utf16Len(s.reopen)
	}
	s.base = s.units
}

func isSpaceRune(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

func runeUnits(r rune) int {
	return max(utf16.RuneLen(r), 1)
}

func utf16Len(text string) int {
	n := 0
	for _, r := range text {
		n += runeUnits(r)
	}
	return n
}

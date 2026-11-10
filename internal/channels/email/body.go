package email

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"unicode"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"golang.org/x/net/html"
)

const (
	maxTurnInputBytes = 65536
	maxPlainBytes     = 2 * maxTurnInputBytes
	maxHTMLBytes      = 4 * maxTurnInputBytes
	maxMIMEDepth      = 8
	maxMIMEParts      = 64
	truncationNotice  = "\n[message truncated]"
)

var errMessageMalformed = errors.New("email: message is not readable")

type textCandidates struct {
	plain string
	html  string
	seen  int
}

func messageBody(raw []byte) (string, error) {
	entity, err := message.Read(bytes.NewReader(raw))
	if entity == nil || (err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err)) {
		return "", errMessageMalformed
	}
	var found textCandidates
	collectText(entity, 0, &found)
	if text := cleanText(found.plain); text != "" {
		return text, nil
	}
	return cleanText(htmlText([]byte(found.html))), nil
}

func collectText(entity *message.Entity, depth int, found *textCandidates) {
	if depth > maxMIMEDepth || found.seen >= maxMIMEParts {
		return
	}
	found.seen++
	if reader := entity.MultipartReader(); reader != nil {
		for {
			child, err := reader.NextPart()
			if child == nil {
				return
			}
			if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
				return
			}
			collectText(child, depth+1, found)
		}
	}
	if disposition, _, err := entity.Header.ContentDisposition(); err == nil && disposition == "attachment" {
		return
	}
	mediaType, _, err := entity.Header.ContentType()
	if err != nil {
		return
	}
	switch mediaType {
	case "text/plain", "":
		if strings.TrimSpace(found.plain) == "" {
			found.plain = readPart(entity.Body, maxPlainBytes)
		}
	case "text/html":
		if strings.TrimSpace(found.html) == "" {
			found.html = readPart(entity.Body, maxHTMLBytes)
		}
	}
}

func readPart(body io.Reader, limit int64) string {
	data, err := io.ReadAll(io.LimitReader(body, limit))
	if err != nil {
		return ""
	}
	return string(data)
}

func htmlText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	tokens := html.NewTokenizer(bytes.NewReader(raw))
	var out strings.Builder
	hidden := 0
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			return out.String()
		case html.CommentToken, html.DoctypeToken:
		case html.TextToken:
			if hidden == 0 {
				out.Write(tokens.Text())
			}
		case html.StartTagToken:
			name, _ := tokens.TagName()
			if isHiddenTag(string(name)) {
				hidden++
			} else if isBlockTag(string(name)) {
				out.WriteByte('\n')
			}
		case html.SelfClosingTagToken:
			name, _ := tokens.TagName()
			if isBlockTag(string(name)) {
				out.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := tokens.TagName()
			if isHiddenTag(string(name)) {
				hidden = max(hidden-1, 0)
			} else if isBlockTag(string(name)) {
				out.WriteByte('\n')
			}
		}
	}
}

func isHiddenTag(name string) bool {
	switch name {
	case "script", "style", "head", "title":
		return true
	}
	return false
}

func isBlockTag(name string) bool {
	switch name {
	case "br", "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre", "table":
		return true
	}
	return false
}

func cleanText(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(strings.ToValidUTF8(text, ""), "\r\n", "\n"), "\r", "\n")
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, text)
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	text = strings.Join(lines, "\n")
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(text)
}

func turnInput(subject, body string) string {
	text := body
	switch {
	case subject != "" && body != "":
		text = "Subject: " + subject + "\n\n" + body
	case subject != "":
		text = "Subject: " + subject
	}
	if len(text) > maxTurnInputBytes {
		return truncateUTF8(text, maxTurnInputBytes) + truncationNotice
	}
	return text
}

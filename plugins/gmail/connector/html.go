package connector

import (
	"bytes"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

type frame struct {
	name   string
	hidden bool
}

var (
	voidTags = map[string]bool{
		"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true, "img": true,
		"input": true, "link": true, "meta": true, "param": true, "source": true, "track": true, "wbr": true,
	}
	hiddenTags = map[string]bool{
		"script": true, "style": true, "head": true, "title": true, "template": true, "noscript": true,
		"iframe": true, "object": true, "embed": true, "svg": true, "math": true, "textarea": true,
	}
	foreignTags = map[string]bool{"svg": true, "math": true}
	blockTags   = map[string]bool{
		"br": true, "p": true, "div": true, "li": true, "ul": true, "ol": true, "tr": true, "table": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "blockquote": true,
		"pre": true, "hr": true, "section": true, "article": true, "header": true, "footer": true,
	}
)

type reducer struct {
	out    strings.Builder
	stack  []frame
	hidden int
}

func htmlText(markup []byte) (string, bool) {
	r := &reducer{}
	tokens := html.NewTokenizer(bytes.NewReader(markup))
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			return r.out.String(), true
		case html.CommentToken, html.DoctypeToken:
		case html.TextToken:
			if r.hidden == 0 {
				r.text(string(tokens.Text()))
			}
		case html.StartTagToken:
			name, hasAttr := tokens.TagName()
			if !r.start(tokens, string(name), hasAttr, false) {
				return r.out.String(), false
			}
		case html.SelfClosingTagToken:
			name, hasAttr := tokens.TagName()
			if !r.start(tokens, string(name), hasAttr, true) {
				return r.out.String(), false
			}
		case html.EndTagToken:
			name, _ := tokens.TagName()
			r.end(string(name))
		}
		if r.out.Len() >= maxTextBytes {
			return r.out.String(), false
		}
	}
}

func (r *reducer) start(tokens *html.Tokenizer, name string, hasAttr, selfClosing bool) bool {
	attrs := map[string]string{}
	for hasAttr {
		var key, value []byte
		key, value, hasAttr = tokens.TagAttr()
		if _, seen := attrs[string(key)]; !seen {
			attrs[string(key)] = string(value)
		}
	}
	if name == "body" {
		r.closeHead()
	}
	hide := hiddenElement(name, attrs)
	if blockTags[name] && r.hidden == 0 {
		r.newline()
	}
	closed := selfClosing && (!hide || foreignTags[name])
	if voidTags[name] || closed || (!hide && r.hidden == 0) {
		return true
	}
	if len(r.stack) >= maxHTMLDepth {
		return false
	}
	r.stack = append(r.stack, frame{name: name, hidden: hide})
	if hide {
		r.hidden++
	}
	return true
}

func (r *reducer) closeHead() {
	for i := len(r.stack) - 1; i >= 0; i-- {
		if r.stack[i].name == "head" {
			r.pop(i)
			return
		}
	}
}

func (r *reducer) end(name string) {
	for i := len(r.stack) - 1; i >= 0; i-- {
		if r.stack[i].name == name {
			r.pop(i)
			break
		}
	}
	if blockTags[name] && r.hidden == 0 {
		r.newline()
	}
}

func (r *reducer) pop(index int) {
	for _, closed := range r.stack[index:] {
		if closed.hidden {
			r.hidden--
		}
	}
	r.stack = r.stack[:index]
}

func (r *reducer) newline() {
	current := r.out.String()
	if current == "" || strings.HasSuffix(current, "\n") {
		return
	}
	r.out.WriteByte('\n')
}

func (r *reducer) text(text string) {
	for _, c := range text {
		if !unicode.IsSpace(c) {
			r.out.WriteRune(c)
			continue
		}
		current := r.out.String()
		if current == "" || strings.HasSuffix(current, " ") || strings.HasSuffix(current, "\n") {
			continue
		}
		r.out.WriteByte(' ')
	}
}

func hiddenElement(name string, attrs map[string]string) bool {
	if hiddenTags[name] {
		return true
	}
	if _, present := attrs["hidden"]; present {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(attrs["aria-hidden"]), "true") {
		return true
	}
	if name == "input" && strings.EqualFold(strings.TrimSpace(attrs["type"]), "hidden") {
		return true
	}
	return hiddenStyle(attrs["style"])
}

func hiddenStyle(style string) bool {
	for _, declaration := range strings.Split(style, ";") {
		name, value, found := strings.Cut(declaration, ":")
		if !found {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.ToLower(value)), "!important")))
		switch name {
		case "display":
			if value == "none" {
				return true
			}
		case "visibility":
			if value == "hidden" || value == "collapse" {
				return true
			}
		case "mso-hide":
			if value == "all" {
				return true
			}
		case "opacity":
			if number, err := strconv.ParseFloat(strings.TrimSuffix(value, "%"), 64); err == nil && number == 0 {
				return true
			}
		case "font-size":
			if zeroLength(value) {
				return true
			}
		}
	}
	return false
}

func zeroLength(value string) bool {
	end := 0
	for end < len(value) && (value[end] == '.' || value[end] == '+' || (value[end] >= '0' && value[end] <= '9')) {
		end++
	}
	if end == 0 {
		return false
	}
	number, err := strconv.ParseFloat(value[:end], 64)
	return err == nil && number == 0
}

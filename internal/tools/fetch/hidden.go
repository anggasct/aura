package fetch

import (
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

func droppedElement(node *html.Node) bool {
	switch node.Data {
	case "script", "style", "noscript", "template", "head", "title", "meta", "link", "iframe", "svg", "canvas", "object", "embed", "input", "textarea":
		return true
	}
	return hiddenElement(node)
}

func hiddenElement(node *html.Node) bool {
	for _, attr := range node.Attr {
		switch attr.Key {
		case "hidden":
			return true
		case "aria-hidden":
			if strings.EqualFold(strings.TrimSpace(attr.Val), "true") {
				return true
			}
		case "style":
			if hiddenStyle(attr.Val) {
				return true
			}
		}
	}
	return false
}

func hiddenStyle(style string) bool {
	for _, declaration := range strings.Split(style, ";") {
		name, value, found := strings.Cut(declaration, ":")
		if !found {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.ToLower(value)), "!important"))
		switch name {
		case "display":
			if value == "none" {
				return true
			}
		case "visibility":
			if value == "hidden" || value == "collapse" {
				return true
			}
		case "opacity":
			if zeroNumber(strings.TrimSuffix(value, "%")) {
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

func zeroNumber(value string) bool {
	number, err := strconv.ParseFloat(value, 64)
	return err == nil && number == 0
}

func zeroLength(value string) bool {
	end := 0
	for end < len(value) && (value[end] == '.' || value[end] == '+' || (value[end] >= '0' && value[end] <= '9')) {
		end++
	}
	return end > 0 && zeroNumber(value[:end])
}

package search

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/html"

	"github.com/anggasct/aura/internal/toolbroker"
)

const duckDuckGoEndpoint = "https://html.duckduckgo.com/html/"

type duckDuckGo struct{}

func (duckDuckGo) newRequest(ctx context.Context, query string, _ int, _ string) (*http.Request, error) {
	values := url.Values{}
	values.Set("q", query)
	return newProviderRequest(ctx, http.MethodGet, duckDuckGoEndpoint+"?"+values.Encode(), http.NoBody, "text/html")
}

func (duckDuckGo) parse(body []byte) ([]searchResult, error) {
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, unreadableResponse()
	}
	if findElement(document, func(node *html.Node) bool { return attribute(node, "id") == "challenge-form" }) != nil {
		return nil, toolbroker.Errorf(toolbroker.ResultExecutionFailed, detailBlocked)
	}
	var results []searchResult
	collectResults(document, &results)
	if len(results) == 0 {
		return nil, toolbroker.Errorf(toolbroker.ResultExecutionFailed, detailNoReadable)
	}
	return results, nil
}

func collectResults(node *html.Node, results *[]searchResult) {
	if node.Type == html.ElementNode && hasClass(node, "result") {
		if !hasClass(node, "result--ad") {
			if entry, ok := readResult(node); ok {
				*results = append(*results, entry)
			}
		}
		return
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		collectResults(child, results)
	}
}

func readResult(node *html.Node) (searchResult, bool) {
	link := findElement(node, func(candidate *html.Node) bool { return candidate.Data == "a" && hasClass(candidate, "result__a") })
	if link == nil {
		return searchResult{}, false
	}
	target, ok := resultURL(attribute(link, "href"))
	if !ok {
		return searchResult{}, false
	}
	entry := searchResult{Title: textOf(link), URL: target}
	if snippet := findElement(node, func(candidate *html.Node) bool { return candidate.Data == "a" && hasClass(candidate, "result__snippet") }); snippet != nil {
		entry.Snippet = textOf(snippet)
	}
	return entry, true
}

func resultURL(href string) (string, bool) {
	raw := href
	if parsed, err := url.Parse(href); err == nil {
		if wrapped := parsed.Query().Get("uddg"); wrapped != "" {
			raw = wrapped
		}
	}
	target, err := url.Parse(raw)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return "", false
	}
	return raw, true
}

func findElement(node *html.Node, match func(*html.Node) bool) *html.Node {
	if node.Type == html.ElementNode && match(node) {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findElement(child, match); found != nil {
			return found
		}
	}
	return nil
}

func attribute(node *html.Node, name string) string {
	for _, attr := range node.Attr {
		if attr.Key == name {
			return attr.Val
		}
	}
	return ""
}

func hasClass(node *html.Node, class string) bool {
	return slices.Contains(strings.Fields(attribute(node, "class")), class)
}

func textOf(node *html.Node) string {
	var builder strings.Builder
	var walk func(*html.Node)
	walk = func(current *html.Node) {
		if current.Type == html.TextNode {
			builder.WriteString(current.Data)
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(strings.Fields(builder.String()), " ")
}

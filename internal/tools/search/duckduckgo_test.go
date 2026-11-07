package search

import (
	"net/http"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/toolbroker"
)

const duckDuckGoFixture = `<!DOCTYPE html><html><head><title>go at DuckDuckGo</title></head><body>
<div class="serp__results"><div id="links" class="results">
<div class="result results_links results_links_deep web-result ">
<div class="links_main links_deep result__body">
<h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F&amp;rut=8f26aa">Go <b>Programming</b> Language</a></h2>
<a class="result__snippet" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F&amp;rut=8f26aa">The <b>Go</b> language   builds simple software.</a>
</div></div>
<div class="result results_links results_links_deep result--ad">
<h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/y.js?ad_domain=ads.example&amp;u3=x">Sponsored</a></h2>
<a class="result__snippet" href="//duckduckgo.com/y.js?x">buy now</a>
</div>
<div class="result results_links results_links_deep web-result ">
<h2 class="result__title"><a rel="nofollow" class="result__a" href="javascript:alert(1)">Script link</a></h2>
<a class="result__snippet" href="#">not a page</a>
</div>
<div class="result results_links results_links_deep web-result ">
<h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fpkg.go.dev%2Fcontext&amp;rut=11aa">context package</a></h2>
</div>
<div class="result results_links results_links_deep web-result ">
<h2 class="result__title"><a rel="nofollow" class="result__a" href="https://go.dev/blog">Blog</a></h2>
<a class="result__snippet" href="https://go.dev/blog">The Go Blog</a>
</div>
</div></div></body></html>`

const duckDuckGoChallenge = `<!DOCTYPE html><html><body><div class="anomaly-modal__modal">
<form id="challenge-form" action="//duckduckgo.com/anomaly.js?sv=html&amp;cc=botnet" method="POST"><input type="hidden" name="q" value="go"></form>
</div></body></html>`

func TestDuckDuckGoReadsResultsFromTheMarkup(t *testing.T) {
	got, err := duckDuckGo{}.parse([]byte(duckDuckGoFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []searchResult{
		{Title: "Go Programming Language", URL: "https://go.dev/", Snippet: "The Go language builds simple software."},
		{Title: "context package", URL: "https://pkg.go.dev/context", Snippet: ""},
		{Title: "Blog", URL: "https://go.dev/blog", Snippet: "The Go Blog"},
	}
	if len(got) != len(want) {
		t.Fatalf("results = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDuckDuckGoSkipsAdsAndNonHTTPURLs(t *testing.T) {
	got, err := duckDuckGo{}.parse([]byte(duckDuckGoFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, entry := range got {
		if entry.Title == "Sponsored" || entry.Title == "Script link" || strings.Contains(entry.URL, "duckduckgo.com") {
			t.Fatalf("unexpected result %+v", entry)
		}
	}
}

func TestDuckDuckGoResultWithoutSnippetNeverTakesTheNextOnes(t *testing.T) {
	got, err := duckDuckGo{}.parse([]byte(duckDuckGoFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got[1].Title != "context package" || got[1].Snippet != "" || got[2].Snippet != "The Go Blog" {
		t.Fatalf("results = %+v", got)
	}
}

func TestDuckDuckGoKeepsAURLWithoutAWrapper(t *testing.T) {
	got, err := duckDuckGo{}.parse([]byte(duckDuckGoFixture))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got[2].URL != "https://go.dev/blog" {
		t.Fatalf("URL = %q", got[2].URL)
	}
}

func TestDuckDuckGoChallengePageIsReportedAsBlockedWithOneRequest(t *testing.T) {
	var captured capturedRequest
	options := optionsFor(providerDuckDuckGo)
	options.client = cannedClient(http.StatusAccepted, duckDuckGoChallenge, &captured)
	_, err := callSearch(newAdapter(t, options), `{"query":"go"}`)
	if classOf(err) != toolbroker.ResultExecutionFailed || detailOf(err) != "search provider blocked the request" {
		t.Fatalf("err = %v", err)
	}
	if captured.calls != 1 {
		t.Fatalf("requests = %d, want exactly one", captured.calls)
	}
}

func TestDuckDuckGoQueryEchoedInAPageDoesNotTriggerTheChallenge(t *testing.T) {
	page := strings.Replace(duckDuckGoFixture, "go at DuckDuckGo", `id="challenge-form" challenge-form at DuckDuckGo`, 1)
	page = strings.Replace(page, `<div class="serp__results">`, `<input type="text" name="q" value="challenge-form" data-note="id=&quot;challenge-form&quot;"><div class="serp__results">`, 1)
	options := optionsFor(providerDuckDuckGo)
	options.client = cannedClient(http.StatusOK, page, nil)
	got, err := callSearch(newAdapter(t, options), `{"query":"challenge-form"}`)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(decodeResults(t, got.Output)) != 3 {
		t.Fatalf("output = %s", got.Output)
	}
}

func TestDuckDuckGoPageWithoutReadableResultsIsNotAnEmptyAnswer(t *testing.T) {
	pages := map[string]string{
		"no results markup":   `<html><body><div class="no-results">No results.</div></body></html>`,
		"empty body":          ``,
		"only ads":            `<div class="result result--ad"><a class="result__a" href="https://ads.example/">ad</a></div>`,
		"result without link": `<div class="result"><a class="result__snippet" href="https://x.example/">text</a></div>`,
		"only unusable urls":  `<div class="result"><a class="result__a" href="/relative">relative</a></div>`,
	}
	for name, page := range pages {
		t.Run(name, func(t *testing.T) {
			options := optionsFor(providerDuckDuckGo)
			options.client = cannedClient(http.StatusOK, page, nil)
			_, err := callSearch(newAdapter(t, options), `{"query":"go"}`)
			if classOf(err) != toolbroker.ResultExecutionFailed || detailOf(err) != "search provider returned no readable results" {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestDuckDuckGoStripsTagsFromTitleAndSnippet(t *testing.T) {
	got, err := duckDuckGo{}.parse([]byte(`<div class="result"><a class="result__a" href="https://a.example/x"><b>Bold</b>&amp;<i>it</i> <script>x</script></a><a class="result__snippet" href="https://a.example/x">one <em>two</em>
	three</a></div>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Title != "Bold&it" || got[0].Snippet != "one two three" {
		t.Fatalf("results = %+v", got)
	}
}

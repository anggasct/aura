package toolbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
)

const secretToken = "sk-live-0123456789abcdef"

type webOnlyRun struct {
	harness    *taintHarness
	turnID     string
	relief     bool
	next       int
	fetches    int
	queries    int
	countByKey map[string]int
}

func newWebOnlyBroker(t *testing.T, limits WebOnlyLimits) *taintHarness {
	t.Helper()
	harness := newTaintBroker(t, nil, nil)
	harness.broker.webOnly = limits
	for name, relief := range map[string]*tools.SinkRelief{
		"web_fetch@v1":  {Kind: tools.SinkReliefLinkProvenance, Argument: "url"},
		"web_search@v1": {Kind: tools.SinkReliefBoundedQuery, Argument: "query", MaxBytes: 256},
	} {
		harness.broker.definitions[name].SinkRelief = relief
	}
	harness.broker.RegisterSecret(secretToken)
	return harness
}

func (r *webOnlyRun) call(name, arguments, key string) error {
	r.harness.seq++
	call := sinkCalls[name]
	request := classifiedRequest(name, arguments, principal.ClassOwner, call.capabilities...)
	request.Tainted, request.WebOnlyRelief = true, r.relief
	request.TurnID = r.turnID
	request.RequestID = fmt.Sprintf("request-%d", r.harness.seq)
	request.IdempotencyKey = key
	request.EventSequence = uint64(r.harness.seq)
	if r.countByKey == nil {
		r.countByKey = map[string]int{}
	}
	switch name {
	case "web_fetch":
		if count, seen := r.countByKey[key]; seen {
			request.FetchCount = count
		} else {
			request.FetchCount = r.fetches
			r.countByKey[key] = r.fetches
			r.fetches++
		}
	case "web_search":
		if count, seen := r.countByKey[key]; seen {
			request.QueryCount = count
		} else {
			request.QueryCount = r.queries
			r.countByKey[key] = r.queries
			r.queries++
		}
	}
	_, err := r.harness.broker.Execute(context.Background(), request)
	return err
}

func (r *webOnlyRun) fetch(url string) error {
	r.next++
	return r.call("web_fetch", fmt.Sprintf(`{"url":%q}`, url), fmt.Sprintf("%s-call-%d", r.turnID, r.next))
}

func (r *webOnlyRun) search(query string) error {
	r.next++
	return r.call("web_search", fmt.Sprintf(`{"query":%q}`, query), fmt.Sprintf("%s-call-%d", r.turnID, r.next))
}

func refusedUnattended(err error) bool { return fault.HasCode(err, ResultUntrustedInputNoApprover) }

func TestWebOnlyJobReadsResultPagesUnattended(t *testing.T) {
	harness := newWebOnlyBroker(t, WebOnlyLimits{Fetches: 3, Queries: 2})
	run := &webOnlyRun{harness: harness, turnID: "turn-1", relief: true}
	if err := run.search("go release notes"); err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, url := range []string{"https://example.com/releases", "https://other.example.org/a?b=c", "https://example.net/link-in-a-page"} {
		if err := run.fetch(url); err != nil {
			t.Fatalf("fetch %s: %v", url, err)
		}
	}
	if harness.prompts != 0 || harness.calls["web_fetch"] != 3 || harness.calls["web_search"] != 1 {
		t.Errorf("prompts = %d, fetches = %d, searches = %d; want no card and every call run", harness.prompts, harness.calls["web_fetch"], harness.calls["web_search"])
	}
}

func TestWebOnlyCapsRefusePastTheLimit(t *testing.T) {
	harness := newWebOnlyBroker(t, WebOnlyLimits{Fetches: 2, Queries: 1})
	run := &webOnlyRun{harness: harness, turnID: "turn-1", relief: true}
	for _, url := range []string{"https://example.com/1", "https://example.com/2"} {
		if err := run.fetch(url); err != nil {
			t.Fatalf("fetch within the cap: %v", err)
		}
	}
	if err := run.fetch("https://example.com/3"); !refusedUnattended(err) {
		t.Errorf("fetch past the cap = %v, want %s", err, ResultUntrustedInputNoApprover)
	}
	if err := run.search("first"); err != nil {
		t.Fatalf("search within the cap: %v", err)
	}
	if err := run.search("second"); !refusedUnattended(err) {
		t.Errorf("search past the cap = %v, want %s", err, ResultUntrustedInputNoApprover)
	}
	if harness.calls["web_fetch"] != 2 || harness.calls["web_search"] != 1 || harness.prompts != 0 {
		t.Errorf("fetches = %d, searches = %d, prompts = %d; want 2, 1 and no card", harness.calls["web_fetch"], harness.calls["web_search"], harness.prompts)
	}
	other := &webOnlyRun{harness: harness, turnID: "turn-2", relief: true}
	if err := other.fetch("https://example.com/again"); err != nil {
		t.Errorf("another run shares the cap: %v", err)
	}
	t.Run("a retried call does not spend the cap twice", func(t *testing.T) {
		harness := newWebOnlyBroker(t, WebOnlyLimits{Fetches: 1})
		run := &webOnlyRun{harness: harness, turnID: "turn-retry", relief: true}
		for range 2 {
			if err := run.call("web_fetch", `{"url":"https://example.com/once"}`, "same-call"); err != nil {
				t.Fatalf("fetch: %v", err)
			}
		}
	})
	t.Run("a cap of zero relieves nothing", func(t *testing.T) {
		harness := newWebOnlyBroker(t, WebOnlyLimits{})
		run := &webOnlyRun{harness: harness, turnID: "turn-zero", relief: true}
		if err := run.fetch("https://example.com/1"); !refusedUnattended(err) {
			t.Errorf("fetch = %v, want %s", err, ResultUntrustedInputNoApprover)
		}
	})
}

func TestWebOnlyReliefNeverCoversWhatCouldCarryASecret(t *testing.T) {
	harness := newWebOnlyBroker(t, WebOnlyLimits{Fetches: 10, Queries: 10})
	run := &webOnlyRun{harness: harness, turnID: "turn-1", relief: true}
	exactly250 := "https://example.com/" + strings.Repeat("a", 250-len("https://example.com/"))
	if err := run.fetch(exactly250); err != nil {
		t.Errorf("a 250 byte URL = %v, want it relieved", err)
	}
	refused := map[string]error{
		"a URL with userinfo":       run.fetch("https://user:password@example.com/"),
		"a URL of 251 bytes":        run.fetch(exactly250 + "a"),
		"a URL with a secret":       run.fetch("https://example.com/?key=" + secretToken),
		"a query with a secret":     run.search("deploy with " + secretToken),
		"a query past 256 bytes":    run.search(strings.Repeat("q", 257)),
		"a URL that does not parse": run.fetch("https://exa mple.com/%zz"),
	}
	for name, err := range refused {
		if !refusedUnattended(err) {
			t.Errorf("%s = %v, want %s", name, err, ResultUntrustedInputNoApprover)
		}
	}
	if err := run.search(strings.Repeat("q", 256)); err != nil {
		t.Errorf("a 256 byte query = %v, want it relieved", err)
	}
	if err := run.search(`site:example.com "exact phrase" -excluded`); err != nil {
		t.Errorf("a query with operators = %v, want it relieved", err)
	}
}

func TestWebOnlyReliefIsGivenOnlyToACleanJob(t *testing.T) {
	harness := newWebOnlyBroker(t, WebOnlyLimits{Fetches: 10, Queries: 10})
	run := &webOnlyRun{harness: harness, turnID: "turn-tainted-job", relief: false}
	if err := run.fetch("https://example.com/1"); !refusedUnattended(err) {
		t.Errorf("fetch of a tainted job = %v, want %s", err, ResultUntrustedInputNoApprover)
	}
	if err := run.search("anything"); !refusedUnattended(err) {
		t.Errorf("search of a tainted job = %v, want %s", err, ResultUntrustedInputNoApprover)
	}
	if harness.calls["web_fetch"]+harness.calls["web_search"] != 0 {
		t.Errorf("a tainted job ran %d web calls, want none", harness.calls["web_fetch"]+harness.calls["web_search"])
	}
}

func TestWebOnlyReliefNeedsTheDeclaredArgument(t *testing.T) {
	harness := newWebOnlyBroker(t, WebOnlyLimits{Fetches: 10, Queries: 10})
	harness.broker.definitions["web_fetch@v1"].SinkRelief = nil
	run := &webOnlyRun{harness: harness, turnID: "turn-1", relief: true}
	if err := run.fetch("https://example.com/1"); !refusedUnattended(err) {
		t.Errorf("fetch with no declared relief = %v, want %s", err, ResultUntrustedInputNoApprover)
	}
}

func TestRegisterToolRefusesAnInvalidSinkRelief(t *testing.T) {
	broker, err := New(&Options{})
	if err != nil {
		t.Fatal(err)
	}
	definition := tools.Definition{
		Name: "lookup", Version: tools.Version, ExternalSink: true,
		Schema:     []byte(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		SinkRelief: &tools.SinkRelief{Kind: tools.SinkReliefLinkProvenance, Argument: "q"},
		Validator:  func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	rule := DefaultRule(&definition)
	adapter := func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{}, nil
	}
	if err := broker.RegisterTool(&definition, adapter, &rule); err == nil {
		t.Fatal("RegisterTool accepted a sink relief on an optional argument")
	}
	definition.Schema = []byte(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`)
	if err := broker.RegisterTool(&definition, adapter, &rule); err != nil {
		t.Errorf("RegisterTool refused a valid sink relief: %v", err)
	}
}

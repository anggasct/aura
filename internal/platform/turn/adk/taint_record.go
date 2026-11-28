package runtimeadk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"

	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/toolcall"
	auratools "github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const (
	callTaintActionName     = "taint"
	failClosedFetchCapFloor = 50
	failClosedQueryCapFloor = 20
	failClosedGrantOrdinal  = 2
	webFetchToolName        = "web_fetch"
	webSearchToolName       = "web_search"
)

type reliefLimitProvider interface {
	ReliefLimits() (toolbroker.WebOnlyLimits, toolbroker.TaintedLimits)
}

type callTaintRecord struct {
	Tainted      bool   `json:"tainted"`
	Source       string `json:"source"`
	Detail       string `json:"detail"`
	Link         string `json:"link"`
	Batch        string `json:"batch"`
	FetchCount   int    `json:"fetch_count"`
	QueryCount   int    `json:"query_count"`
	GrantOrdinal int    `json:"grant_ordinal"`
}

func decodeCallTaintRecord(raw []byte) (callTaintRecord, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record callTaintRecord
	if err := decoder.Decode(&record); err != nil {
		return callTaintRecord{}, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return callTaintRecord{}, false
	}
	if record.FetchCount < 0 || record.QueryCount < 0 || record.GrantOrdinal < 0 {
		return callTaintRecord{}, false
	}
	if record.Tainted && record.Source == "" {
		return callTaintRecord{}, false
	}
	if record.Link != "" && !auratools.ValidLinkProvenance(auratools.LinkProvenance(record.Link)) {
		return callTaintRecord{}, false
	}
	return record, true
}

type trackedURLSet struct {
	order []string
	items map[string]struct{}
}

func (s *trackedURLSet) add(canonical string) {
	if canonical == "" {
		return
	}
	if s.items == nil {
		s.items = map[string]struct{}{}
	}
	if _, exists := s.items[canonical]; exists {
		return
	}
	s.items[canonical] = struct{}{}
	s.order = append(s.order, canonical)
	if len(s.order) > auratools.MaxTrackedURLs {
		delete(s.items, s.order[0])
		s.order = s.order[1:]
	}
}

func (s *trackedURLSet) has(canonical string) bool {
	if s == nil {
		return false
	}
	_, ok := s.items[canonical]
	return ok
}

type trackedRecordMap struct {
	order []string
	items map[string]json.RawMessage
}

func (m *trackedRecordMap) put(canonical string, record json.RawMessage) {
	if canonical == "" || len(record) == 0 {
		return
	}
	if m.items == nil {
		m.items = map[string]json.RawMessage{}
	}
	if _, exists := m.items[canonical]; !exists {
		m.order = append(m.order, canonical)
		if len(m.order) > auratools.MaxTrackedURLs {
			delete(m.items, m.order[0])
			m.order = m.order[1:]
		}
	}
	m.items[canonical] = slices.Clone(record)
}

func (m *trackedRecordMap) get(canonical string) (json.RawMessage, bool) {
	if m == nil {
		return nil, false
	}
	rec, ok := m.items[canonical]
	if !ok {
		return nil, false
	}
	return slices.Clone(rec), true
}

type recordedSearch struct {
	callID string
	query  string
	urls   []string
}

type turnTaintTracker struct {
	mu              sync.Mutex
	ownerURLs       trackedURLSet
	fetchedRecords  trackedRecordMap
	searches        []recordedSearch
	searchURLOrder  []string
	searchURLBatch  map[string]string
	fetchCount      int
	queryCount      int
	grantOrdinal    int
	failClosedFetch int
	failClosedQuery int
}

type turnTaintTrackerKey struct{}

func withTurnTaintTracker(ctx context.Context, tracker *turnTaintTracker) context.Context {
	return context.WithValue(ctx, turnTaintTrackerKey{}, tracker)
}

func turnTaintTrackerFromContext(ctx context.Context) *turnTaintTracker {
	tracker, _ := ctx.Value(turnTaintTrackerKey{}).(*turnTaintTracker)
	return tracker
}

func (x *ADKExecutor) newTurnTaintTracker(ctx context.Context, req *turn.TurnRequest) *turnTaintTracker {
	fetchCap := failClosedFetchCapFloor
	queryCap := failClosedQueryCapFloor
	if provider, ok := x.builtins.(reliefLimitProvider); ok {
		webOnly, tainted := provider.ReliefLimits()
		fetchCap = max(fetchCap, webOnly.Fetches, tainted.Fetches)
		queryCap = max(queryCap, webOnly.Queries, tainted.Queries)
	}
	tracker := &turnTaintTracker{
		searchURLBatch:  map[string]string{},
		failClosedFetch: fetchCap,
		failClosedQuery: queryCap,
	}
	if req == nil {
		return tracker
	}
	if x.sessions != nil && req.SessionID != "" {
		if events, err := x.sessions.ListEvents(ctx, req.SessionID, 0, maxEventLoad); err == nil {
			tracker.ingestSessionEvents(events, req.TurnID, req.PrincipalID)
		}
	}
	tracker.ingestCurrentTurnInput(req)
	return tracker
}

type acceptedTurnMeta struct {
	Origin    string `json:"origin"`
	Principal string `json:"principal"`
}

func (t *turnTaintTracker) ingestSessionEvents(events []turn.Event, currentTurnID, principalID string) {
	acceptedByTurn := map[string]acceptedTurnMeta{}
	var prevTurnID string
	var prevTurnSeq uint64
	hasPrevTurn := false

	for i := range events {
		ev := &events[i]
		if ev.Kind != turn.EventKindTurnAccepted {
			continue
		}
		var meta acceptedTurnMeta
		if err := json.Unmarshal(ev.Payload, &meta); err != nil {
			continue
		}
		acceptedByTurn[ev.TurnID] = meta
		if ev.TurnID != currentTurnID && (!hasPrevTurn || ev.Sequence > prevTurnSeq) {
			prevTurnID = ev.TurnID
			prevTurnSeq = ev.Sequence
			hasPrevTurn = true
		}
	}

	fetchCalls := map[string]string{}
	searchCalls := map[string]string{}
	for i := range events {
		ev := &events[i]
		if ev.TurnID == currentTurnID || ev.Kind != turnstore.EventKindADK {
			continue
		}
		adkEv, err := turnstore.RuntimeEventToADK(ev)
		if err != nil || adkEv == nil || adkEv.Content == nil {
			continue
		}
		if adkEv.Author == userAuthor {
			if meta, ok := acceptedByTurn[ev.TurnID]; ok && meta.Principal == principalID && isStepOneOwnerAuthoredOrigin(turn.Origin(meta.Origin)) {
				for _, part := range adkEv.Content.Parts {
					if part == nil || part.Text == "" {
						continue
					}
					for _, u := range auratools.ExtractURLs(stripUntrustedFromOwnerText(part.Text)) {
						t.ownerURLs.add(u)
					}
				}
			}
		}
		for _, part := range adkEv.Content.Parts {
			if part == nil {
				continue
			}
			if fc := part.FunctionCall; fc != nil && fc.ID != "" {
				switch fc.Name {
				case webFetchToolName:
					if rawURL, ok := fc.Args["url"].(string); ok {
						fetchCalls[fc.ID] = rawURL
					}
				case webSearchToolName:
					if query, ok := fc.Args["query"].(string); ok {
						searchCalls[fc.ID] = query
					}
				}
			}
			if fr := part.FunctionResponse; fr != nil && fr.ID != "" {
				t.ingestFunctionResponse(ev.TurnID, prevTurnID, fr, fetchCalls, searchCalls)
			}
		}
	}
}

func (t *turnTaintTracker) ingestFunctionResponse(turnID, prevTurnID string, fr *genai.FunctionResponse, fetchCalls, searchCalls map[string]string) {
	if fr == nil || fr.ID == "" || hasErrorField(fr.Response) {
		return
	}
	switch fr.Name {
	case webFetchToolName:
		if requestedURL, paired := fetchCalls[fr.ID]; paired {
			t.recordFetchedResponse(requestedURL, fr.Response)
		}
	case webSearchToolName:
		if prevTurnID == "" || turnID != prevTurnID {
			return
		}
		if query, paired := searchCalls[fr.ID]; paired {
			raw, err := json.Marshal(fr.Response)
			if err != nil {
				return
			}
			t.recordSearchOutput(fr.ID, query, raw)
		}
	}
}

func (t *turnTaintTracker) recordFetchedResponse(requestedURL string, response map[string]any) {
	if hasErrorField(response) {
		return
	}
	raw, err := json.Marshal(response)
	if err != nil {
		return
	}
	t.recordFetchedOutput(requestedURL, raw)
}

func (t *turnTaintTracker) recordFetchedOutput(requestedURL string, output json.RawMessage) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(output, &fields); err != nil || fields == nil {
		return
	}
	if _, hasErr := fields["error"]; hasErr {
		return
	}
	if canonical, ok := auratools.CanonicalURL(requestedURL); ok {
		t.fetchedRecords.put(canonical, output)
	}
	if respURLRaw, ok := fields["url"]; ok {
		var respURL string
		if json.Unmarshal(respURLRaw, &respURL) == nil {
			if canonical, ok := auratools.CanonicalURL(respURL); ok {
				t.fetchedRecords.put(canonical, output)
			}
		}
	}
}

func (t *turnTaintTracker) recordSearchOutput(callID, query string, output json.RawMessage) {
	if callID == "" {
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(output, &fields); err != nil || fields == nil {
		return
	}
	if _, hasErr := fields["error"]; hasErr {
		return
	}
	var parsed struct {
		Results []struct {
			URL string `json:"url"`
		} `json:"results"`
	}
	if err := json.Unmarshal(output, &parsed); err != nil {
		return
	}
	urls := make([]string, 0, len(parsed.Results))
	for i := range parsed.Results {
		canonical, ok := auratools.CanonicalURL(parsed.Results[i].URL)
		if !ok {
			continue
		}
		urls = append(urls, canonical)
		t.recordSearchURL(canonical, callID)
	}
	t.searches = append(t.searches, recordedSearch{
		callID: callID,
		query:  query,
		urls:   urls,
	})
}

func (t *turnTaintTracker) recordSearchURL(canonical, callID string) {
	if _, exists := t.searchURLBatch[canonical]; !exists {
		t.searchURLOrder = append(t.searchURLOrder, canonical)
		if len(t.searchURLOrder) > auratools.MaxTrackedURLs {
			delete(t.searchURLBatch, t.searchURLOrder[0])
			t.searchURLOrder = t.searchURLOrder[1:]
		}
	}
	t.searchURLBatch[canonical] = callID
}

func (t *turnTaintTracker) ingestCurrentTurnInput(req *turn.TurnRequest) {
	ownerOrigin := isStepOneOwnerAuthoredOrigin(req.Origin)
	cleanFullJob := req.Origin == turn.OriginSchedule && req.JobTaint == "" && !req.WebOnly
	if !ownerOrigin && !cleanFullJob {
		return
	}
	for i := range req.Parts {
		part := &req.Parts[i]
		if part.Source != "" || len(part.Image) > 0 || part.Text == "" {
			continue
		}
		for _, u := range auratools.ExtractURLs(stripUntrustedFromOwnerText(part.Text)) {
			t.ownerURLs.add(u)
		}
	}
}

func isStepOneOwnerAuthoredOrigin(origin turn.Origin) bool {
	if origin == turn.OriginTerminal {
		return true
	}
	channel, account, found := strings.Cut(string(origin), ":")
	if !found || account == "" {
		return false
	}
	return channel == principal.ChannelTelegram || channel == principal.ChannelDiscord
}

var (
	untrustedInputOpenPattern   = regexp.MustCompile(`(?i)\[untrusted_input\b[^\]]*\]`)
	untrustedInputClosePattern  = regexp.MustCompile(`(?i)\[/untrusted_input\]`)
	untrustedRecallOpenPattern  = regexp.MustCompile(`(?i)\[untrusted recall evidence\b[^\]]*\]`)
	untrustedRecallClosePattern = regexp.MustCompile(`(?i)\[/untrusted recall evidence\]`)
	untrustedProfOpenPattern    = regexp.MustCompile(`(?i)\[untrusted profile context\b[^\]]*\]`)
	untrustedProfClosePattern   = regexp.MustCompile(`(?i)\[/untrusted profile context\]`)
	untrustedSkillOpenPattern   = regexp.MustCompile(`(?i)\[untrusted_skill_context\b[^\]]*\]`)
	untrustedSkillClosePattern  = regexp.MustCompile(`(?i)\[/untrusted_skill_context\]`)
)

func stripUntrustedFromOwnerText(text string) string {
	cleaned := stripTaggedBlocks(text, untrustedInputOpenPattern, untrustedInputClosePattern)
	cleaned = stripTaggedBlocks(cleaned, untrustedRecallOpenPattern, untrustedRecallClosePattern)
	cleaned = stripTaggedBlocks(cleaned, untrustedProfOpenPattern, untrustedProfClosePattern)
	cleaned = stripTaggedBlocks(cleaned, untrustedSkillOpenPattern, untrustedSkillClosePattern)
	lines := strings.Split(cleaned, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[attachment ") && strings.Contains(trimmed, "artifact:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func stripTaggedBlocks(text string, openRe, closeRe *regexp.Regexp) string {
	var out strings.Builder
	remaining := text
	for {
		openLoc := openRe.FindStringIndex(remaining)
		if openLoc == nil {
			out.WriteString(remaining)
			return out.String()
		}
		out.WriteString(remaining[:openLoc[0]])
		afterOpen := remaining[openLoc[1]:]
		closeLoc := closeRe.FindStringIndex(afterOpen)
		if closeLoc == nil {
			return out.String()
		}
		remaining = afterOpen[closeLoc[1]:]
	}
}

func (t *turnTaintTracker) prepareCall(ctx context.Context, slot *toolcall.Slot, request *BuiltinToolRequest, definition *BuiltinToolDefinition) {
	if t == nil || request == nil || definition == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	compute := func() callTaintRecord {
		rec := callTaintRecord{
			Tainted:      request.Tainted,
			Source:       string(request.TaintMark.Source),
			Detail:       request.TaintMark.Detail,
			Link:         string(auratools.LinkProvenanceNone),
			FetchCount:   t.fetchCount,
			QueryCount:   t.queryCount,
			GrantOrdinal: 0,
		}
		if definition.LinkArgument != "" {
			link, batchID, _ := t.resolveLinkLocked(request.Arguments, definition.LinkArgument)
			rec.Link = string(link)
			rec.Batch = batchID
		}
		if request.Tainted && definition.Risk == auratools.RiskTrustGrant {
			rec.GrantOrdinal = t.grantOrdinal + 1
		}
		return rec
	}

	var rec callTaintRecord
	valid := true
	if scope, scoped := durable.TurnScopeFrom(ctx); scoped && slot != nil {
		raw, err := scope.Invocation().RunAction(ctx, slot.Action(callTaintActionName), func(context.Context) ([]byte, error) {
			return json.Marshal(compute())
		})
		if err != nil {
			valid = false
		} else {
			rec, valid = decodeCallTaintRecord(raw)
		}
	} else {
		rec = compute()
	}

	if !valid {
		t.applyFailClosedLocked(request)
		return
	}

	t.applyRecordLocked(request, definition, &rec)
}

func (t *turnTaintTracker) applyFailClosedLocked(request *BuiltinToolRequest) {
	mark := turn.TaintMark{Source: turn.TaintSourceLegacy}
	request.Tainted = true
	request.TaintMark = mark
	request.TaintSource = mark.Label()
	request.LinkProvenance = auratools.LinkProvenanceNone
	request.ResultBatch = nil
	request.SessionRecord = nil
	request.WebOnlyRelief = false
	request.FetchCount = t.failClosedFetch
	request.QueryCount = t.failClosedQuery
	request.TrustGrantOrdinal = failClosedGrantOrdinal
	if t.failClosedFetch > t.fetchCount {
		t.fetchCount = t.failClosedFetch
	}
	if t.failClosedQuery > t.queryCount {
		t.queryCount = t.failClosedQuery
	}
	if failClosedGrantOrdinal > t.grantOrdinal {
		t.grantOrdinal = failClosedGrantOrdinal
	}
}

func (t *turnTaintTracker) applyRecordLocked(request *BuiltinToolRequest, definition *BuiltinToolDefinition, rec *callTaintRecord) {
	mark := turn.TaintMark{
		Source: turn.TaintSource(rec.Source),
		Detail: rec.Detail,
	}
	request.Tainted = rec.Tainted
	request.TaintMark = mark
	if rec.Tainted {
		request.TaintSource = mark.Label()
	} else {
		request.TaintSource = ""
	}
	link := auratools.LinkProvenance(rec.Link)
	if link == "" {
		link = auratools.LinkProvenanceNone
	}
	request.LinkProvenance = link
	request.FetchCount = rec.FetchCount
	request.QueryCount = rec.QueryCount
	request.TrustGrantOrdinal = rec.GrantOrdinal
	request.SessionRecord = nil
	request.ResultBatch = nil

	if definition.LinkArgument != "" {
		if rawURL, ok := extractStringArg(request.Arguments, definition.LinkArgument); ok {
			if canonical, ok := auratools.CanonicalURL(rawURL); ok {
				if link == auratools.LinkProvenanceFetched {
					if stored, found := t.fetchedRecords.get(canonical); found {
						request.SessionRecord = stored
					}
				}
				if link == auratools.LinkProvenanceSearchResult {
					request.ResultBatch = t.buildResultBatchLocked(rec.Batch)
				}
			}
		}
		if rec.Tainted && t.shouldCountFetch(request.WebOnlyRelief, link, request.Arguments, definition.LinkArgument) {
			next := rec.FetchCount + 1
			if next > t.fetchCount {
				t.fetchCount = next
			}
		}
	}

	if definition.QueryArgument != "" && rec.Tainted {
		if t.shouldCountQuery(request.WebOnlyRelief, request.Arguments, definition.QueryArgument, definition.QueryMaxBytes) {
			next := rec.QueryCount + 1
			if next > t.queryCount {
				t.queryCount = next
			}
		}
	}

	if rec.GrantOrdinal > t.grantOrdinal {
		t.grantOrdinal = rec.GrantOrdinal
	}
}

func (t *turnTaintTracker) shouldCountFetch(webOnly bool, link auratools.LinkProvenance, args json.RawMessage, argName string) bool {
	if webOnly {
		rawURL, ok := extractStringArg(args, argName)
		if !ok || len(rawURL) > 250 {
			return false
		}
		_, valid := auratools.CanonicalURL(rawURL)
		return valid
	}
	return link == auratools.LinkProvenanceOwnerMessage || link == auratools.LinkProvenanceSearchResult
}

func (t *turnTaintTracker) shouldCountQuery(webOnly bool, args json.RawMessage, argName string, maxBytes int) bool {
	query, ok := extractStringArg(args, argName)
	if !ok {
		return false
	}
	if maxBytes > 0 && len(query) > maxBytes {
		return false
	}
	if webOnly {
		return true
	}
	lower := strings.ToLower(query)
	if strings.Contains(lower, "://") || strings.Contains(lower, "www.") {
		return false
	}
	for _, op := range queryOperators {
		offset := 0
		for offset < len(lower) {
			idx := strings.Index(lower[offset:], op)
			if idx < 0 {
				break
			}
			pos := offset + idx
			if pos == 0 || !isASCIILetterOrDigit(lower[pos-1]) {
				return false
			}
			offset = pos + len(op)
		}
	}
	return true
}

var queryOperators = []string{
	"site:", "inurl:", "intitle:", "intext:", "allinurl:", "allintitle:", "allintext:",
	"link:", "related:", "cache:", "filetype:", "ext:", "inbody:", "domain:",
}

func isASCIILetterOrDigit(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

func (t *turnTaintTracker) resolveLinkLocked(args json.RawMessage, argName string) (auratools.LinkProvenance, string, json.RawMessage) {
	rawURL, ok := extractStringArg(args, argName)
	if !ok {
		return auratools.LinkProvenanceNone, "", nil
	}
	canonical, ok := auratools.CanonicalURL(rawURL)
	if !ok {
		return auratools.LinkProvenanceNone, "", nil
	}
	if stored, found := t.fetchedRecords.get(canonical); found {
		return auratools.LinkProvenanceFetched, "", stored
	}
	if t.ownerURLs.has(canonical) {
		return auratools.LinkProvenanceOwnerMessage, "", nil
	}
	if batchID, found := t.searchURLBatch[canonical]; found {
		return auratools.LinkProvenanceSearchResult, batchID, nil
	}
	return auratools.LinkProvenanceNone, "", nil
}

func (t *turnTaintTracker) buildResultBatchLocked(callID string) *auratools.ResultBatch {
	if len(t.searches) == 0 {
		return nil
	}
	queries := make([]string, 0, len(t.searches))
	var urls []string
	for i := range t.searches {
		queries = append(queries, t.searches[i].query)
		urls = append(urls, t.searches[i].urls...)
	}
	return &auratools.ResultBatch{
		CallID:  callID,
		Queries: queries,
		URLs:    urls,
	}
}

func (t *turnTaintTracker) observeResult(definition *BuiltinToolDefinition, callID string, args, output json.RawMessage) {
	if t == nil || definition == nil || len(output) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case definition.LinkArgument != "":
		if rawURL, ok := extractStringArg(args, definition.LinkArgument); ok {
			t.recordFetchedOutput(rawURL, output)
		}
	case definition.QueryArgument != "":
		if query, ok := extractStringArg(args, definition.QueryArgument); ok {
			t.recordSearchOutput(callID, query, output)
		}
	}
}

func extractStringArg(args json.RawMessage, name string) (string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return "", false
	}
	var value string
	if err := json.Unmarshal(fields[name], &value); err != nil {
		return "", false
	}
	return value, true
}

func hasErrorField(m map[string]any) bool {
	if m == nil {
		return false
	}
	v, ok := m["error"]
	return ok && v != nil && v != ""
}

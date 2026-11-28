package runtimeadk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/toolcall"
	auratools "github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

const (
	admissionTaintAction    = "taint-admission"
	callTaintActionName     = "taint"
	failClosedFetchCapFloor = 50
	failClosedQueryCapFloor = 20
	failClosedGrantOrdinal  = 2
	webFetchToolName        = "web_fetch"
	webSearchToolName       = "web_search"
)

type admissionTaintRecord struct {
	Tainted bool   `json:"tainted"`
	Source  string `json:"source"`
	Detail  string `json:"detail"`
}

type admissionTaintRecordWire struct {
	Tainted *bool  `json:"tainted"`
	Source  string `json:"source"`
	Detail  string `json:"detail"`
}

func decodeAdmissionTaintRecord(raw []byte) (admissionTaintRecord, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return admissionTaintRecord{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var wire admissionTaintRecordWire
	if err := decoder.Decode(&wire); err != nil {
		return admissionTaintRecord{}, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return admissionTaintRecord{}, false
	}
	if wire.Tainted == nil {
		return admissionTaintRecord{}, false
	}
	rec := admissionTaintRecord{
		Tainted: *wire.Tainted,
		Source:  wire.Source,
		Detail:  wire.Detail,
	}
	if rec.Tainted {
		if !turn.IsValidTaintSource(turn.TaintSource(rec.Source)) {
			return admissionTaintRecord{}, false
		}
	} else if rec.Source != "" || rec.Detail != "" {
		return admissionTaintRecord{}, false
	}
	return rec, true
}

type taintSnapshotModel struct {
	inner   adkmodel.LLM
	tracker *turnTaintTracker
}

func snapshotTaintModel(inner adkmodel.LLM, tracker *turnTaintTracker) adkmodel.LLM {
	if tracker == nil {
		return inner
	}
	return &taintSnapshotModel{inner: inner, tracker: tracker}
}

func (m *taintSnapshotModel) Name() string { return m.inner.Name() }

func (m *taintSnapshotModel) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.tracker.snapshotForModelCall()
	return m.inner.GenerateContent(ctx, req, stream)
}

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

type callTaintRecordWire struct {
	Tainted      *bool  `json:"tainted"`
	Source       string `json:"source"`
	Detail       string `json:"detail"`
	Link         string `json:"link"`
	Batch        string `json:"batch"`
	FetchCount   int    `json:"fetch_count"`
	QueryCount   int    `json:"query_count"`
	GrantOrdinal int    `json:"grant_ordinal"`
}

func decodeCallTaintRecord(raw []byte) (callTaintRecord, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return callTaintRecord{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var wire callTaintRecordWire
	if err := decoder.Decode(&wire); err != nil {
		return callTaintRecord{}, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return callTaintRecord{}, false
	}
	if wire.Tainted == nil {
		return callTaintRecord{}, false
	}
	record := callTaintRecord{
		Tainted:      *wire.Tainted,
		Source:       wire.Source,
		Detail:       wire.Detail,
		Link:         wire.Link,
		Batch:        wire.Batch,
		FetchCount:   wire.FetchCount,
		QueryCount:   wire.QueryCount,
		GrantOrdinal: wire.GrantOrdinal,
	}
	if record.FetchCount < 0 || record.QueryCount < 0 || record.GrantOrdinal < 0 {
		return callTaintRecord{}, false
	}
	if record.Tainted && record.Source == "" {
		return callTaintRecord{}, false
	}
	if !auratools.ValidLinkProvenance(auratools.LinkProvenance(record.Link)) {
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

type pendingTaintMark struct {
	callID string
	mark   turn.TaintMark
}

type turnTaintTracker struct {
	mu              sync.Mutex
	currentTainted  bool
	currentMark     turn.TaintMark
	snapshotTainted bool
	snapshotMark    turn.TaintMark
	markWritten     bool
	pendingMarks    []pendingTaintMark
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

func (t *turnTaintTracker) initAdmission(
	ctx context.Context,
	req *turn.TurnRequest,
	blocks []turn.ContextBlock,
	sessionLookup func() (turn.TaintMark, bool, error),
) (turn.TaintMark, bool) {
	compute := func() admissionTaintRecord {
		if mark, tainted := req.AdmissionTaint(); tainted {
			return admissionTaintRecord{Tainted: true, Source: string(mark.Source), Detail: mark.Detail}
		}
		if sessionLookup != nil {
			found, ok, err := sessionLookup()
			if err != nil {
				return admissionTaintRecord{Tainted: true, Source: string(turn.TaintSourceLegacy)}
			}
			if ok {
				return admissionTaintRecord{Tainted: true, Source: string(turn.TaintSourceSession), Detail: string(found.Source)}
			}
		}
		for i := range blocks {
			if blocks[i].Taint != nil {
				return admissionTaintRecord{
					Tainted: true,
					Source:  string(blocks[i].Taint.Source),
					Detail:  blocks[i].Taint.Detail,
				}
			}
		}
		return admissionTaintRecord{}
	}

	var rec admissionTaintRecord
	valid := true
	if scope, scoped := durable.TurnScopeFrom(ctx); scoped {
		raw, err := scope.Invocation().RunAction(ctx, admissionTaintAction, func(context.Context) ([]byte, error) {
			return json.Marshal(compute())
		})
		if err != nil {
			valid = false
		} else {
			rec, valid = decodeAdmissionTaintRecord(raw)
		}
	} else {
		rec = compute()
	}

	var mark turn.TaintMark
	var tainted bool
	if !valid {
		tainted = true
		mark = turn.TaintMark{Source: turn.TaintSourceLegacy}
	} else if rec.Tainted {
		tainted = true
		mark = turn.TaintMark{Source: turn.TaintSource(rec.Source), Detail: rec.Detail}
	}

	t.mu.Lock()
	t.currentTainted = tainted
	t.currentMark = mark
	t.snapshotTainted = tainted
	t.snapshotMark = mark
	if tainted {
		t.markWritten = true
	}
	t.mu.Unlock()
	return mark, tainted
}

func (t *turnTaintTracker) snapshotForModelCall() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.snapshotTainted = t.currentTainted
	t.snapshotMark = t.currentMark
	t.mu.Unlock()
}

func (t *turnTaintTracker) currentState() (turn.TaintMark, bool) {
	if t == nil {
		return turn.TaintMark{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.currentMark, t.currentTainted
}

func (t *turnTaintTracker) markAdmissionWritten() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.markWritten = true
	t.mu.Unlock()
}

func (t *turnTaintTracker) takePendingMark(ev *session.Event) (turn.TaintMark, bool) {
	if t == nil || ev == nil || ev.Content == nil {
		return turn.TaintMark{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.markWritten || len(t.pendingMarks) == 0 {
		return turn.TaintMark{}, false
	}
	for _, part := range ev.Content.Parts {
		if part == nil || part.FunctionResponse == nil {
			continue
		}
		for i := range t.pendingMarks {
			if t.pendingMarks[i].callID == part.FunctionResponse.ID {
				mark := t.pendingMarks[i].mark
				t.markWritten = true
				t.pendingMarks = nil
				return mark, true
			}
		}
	}
	return turn.TaintMark{}, false
}

func (t *turnTaintTracker) takeAnyPendingMark() (turn.TaintMark, bool) {
	if t == nil {
		return turn.TaintMark{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.markWritten || len(t.pendingMarks) == 0 {
		return turn.TaintMark{}, false
	}
	mark := t.pendingMarks[0].mark
	t.markWritten = true
	t.pendingMarks = nil
	return mark, true
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

	requestTaintedTurn := map[string]bool{}
	for i := range events {
		ev := &events[i]
		if ev.Kind == turn.EventKindSessionTainted {
			mark, ok := turn.DecodeTaintMark(ev.Payload)
			if !ok || isWholeTurnRequestSource(mark.Source) || mark.Source == turn.TaintSourceOtherPrincipal {
				requestTaintedTurn[ev.TurnID] = true
			}
			continue
		}
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
		if err != nil || adkEv == nil || adkEv.Partial || adkEv.Content == nil {
			continue
		}
		if adkEv.Author == userAuthor {
			if meta, ok := acceptedByTurn[ev.TurnID]; ok && meta.Principal == principalID && !requestTaintedTurn[ev.TurnID] && isOwnerAuthoredOrigin(turn.Origin(meta.Origin)) {
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
			if fc := part.FunctionCall; fc != nil && adkEv.Author != userAuthor && strings.HasPrefix(fc.ID, callIDPrefix) {
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
	if req == nil || !isOwnerAuthoredOrigin(req.Origin) {
		return
	}
	if mark, tainted := req.AdmissionTaint(); tainted && isWholeTurnRequestSource(mark.Source) {
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

func isWholeTurnRequestSource(source turn.TaintSource) bool {
	switch source {
	case turn.TaintSourceOrigin,
		turn.TaintSourceClientHistory,
		turn.TaintSourceWorkflowInput,
		turn.TaintSourceScheduleOrigin,
		turn.TaintSourceWebOnly,
		turn.TaintSourceKnowledgeScope:
		return true
	default:
		return false
	}
}

func isOwnerAuthoredOrigin(origin turn.Origin) bool {
	if origin == turn.OriginTerminal || origin == turn.OriginHTTP || origin == turn.OriginSchedule {
		return true
	}
	channel, account, found := strings.Cut(string(origin), ":")
	if !found || account == "" {
		return false
	}
	return channel == principal.ChannelTelegram || channel == principal.ChannelDiscord || channel == principal.ChannelEmail
}

var (
	untrustedInputOpenPattern      = regexp.MustCompile(`(?i)\[untrusted_input\b[^\]]*\]`)
	untrustedInputClosePattern     = regexp.MustCompile(`(?i)\[/untrusted_input\]`)
	untrustedKnowledgeOpenPattern  = regexp.MustCompile(`(?i)\[untrusted_knowledge\b[^\]]*\]`)
	untrustedKnowledgeClosePattern = regexp.MustCompile(`(?i)\[/untrusted_knowledge\]`)
	untrustedRecallOpenPattern     = regexp.MustCompile(`(?i)\[untrusted recall evidence\b[^\]]*\]`)
	untrustedRecallClosePattern    = regexp.MustCompile(`(?i)\[/untrusted recall evidence\]`)
	untrustedProfOpenPattern       = regexp.MustCompile(`(?i)\[untrusted profile context\b[^\]]*\]`)
	untrustedProfClosePattern      = regexp.MustCompile(`(?i)\[/untrusted profile context\]`)
	untrustedSkillOpenPattern      = regexp.MustCompile(`(?i)\[untrusted_skill_context\b[^\]]*\]`)
	untrustedSkillClosePattern     = regexp.MustCompile(`(?i)\[/untrusted_skill_context\]`)
)

func stripUntrustedFromOwnerText(text string) string {
	cleaned := stripTaggedBlocks(text, untrustedInputOpenPattern, untrustedInputClosePattern)
	cleaned = stripTaggedBlocks(cleaned, untrustedKnowledgeOpenPattern, untrustedKnowledgeClosePattern)
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
			Tainted:      t.snapshotTainted,
			Source:       string(t.snapshotMark.Source),
			Detail:       t.snapshotMark.Detail,
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
		if t.snapshotTainted && definition.Risk == auratools.RiskTrustGrant {
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
	if !webOnly && link != auratools.LinkProvenanceOwnerMessage && link != auratools.LinkProvenanceSearchResult {
		return false
	}
	rawURL, ok := extractStringArg(args, argName)
	if !ok || len(rawURL) > auratools.MaxReliefURLBytes {
		return false
	}
	_, valid := auratools.CanonicalURL(rawURL)
	return valid
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
	return !auratools.QueryHasURLPattern(query) && !auratools.QueryHasSearchOperator(query)
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

func (t *turnTaintTracker) observeResult(definition *BuiltinToolDefinition, callID string, args json.RawMessage, res BuiltinToolResult, callErr error) {
	if t == nil || definition == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if callErr == nil && len(res.Output) > 0 {
		switch {
		case definition.LinkArgument != "":
			if rawURL, ok := extractStringArg(args, definition.LinkArgument); ok {
				t.recordFetchedOutput(rawURL, res.Output)
			}
		case definition.QueryArgument != "":
			if query, ok := extractStringArg(args, definition.QueryArgument); ok {
				t.recordSearchOutput(callID, query, res.Output)
			}
		}
	}
	var taints bool
	if callErr == nil {
		taints = res.Untrusted || res.WorkspaceTainted
	} else {
		taints = fault.HasCode(callErr, toolbroker.ResultExecutionFailed) && definition.OutputCanTaint
	}
	if !taints {
		return
	}
	var mark turn.TaintMark
	if callErr == nil && res.WorkspaceTainted {
		mark = turn.TaintMark{Source: turn.TaintSourceWorkspace}
	} else {
		mark = turn.TaintMark{
			Source: turn.TaintSourceToolResult,
			Detail: resultTaintDetail(definition.Name, args),
		}
	}
	if !t.currentTainted {
		t.currentTainted = true
		t.currentMark = mark
	}
	if !t.markWritten && callID != "" {
		updated := false
		for i := range t.pendingMarks {
			if t.pendingMarks[i].callID == callID {
				t.pendingMarks[i].mark = mark
				updated = true
				break
			}
		}
		if !updated {
			t.pendingMarks = append(t.pendingMarks, pendingTaintMark{callID: callID, mark: mark})
		}
	}
}

func resultTaintDetail(toolName string, args json.RawMessage) string {
	if toolName != webFetchToolName {
		return toolName
	}
	rawURL, ok := extractStringArg(args, "url")
	if !ok {
		return toolName
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() == "" {
		return toolName
	}
	return toolName + " " + parsed.Hostname()
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

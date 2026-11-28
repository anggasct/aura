package toolbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	maxReliefURLBytes = tools.MaxReliefURLBytes
	maxBatchURLs      = 20
	trackedBatchTurns = 256
	resultBatchAction = "turn/result-batch"

	reliefLinkOwnerMessage = "link_owner_message"
	reliefLinkFetched      = "link_fetched_record"
	reliefSearchBatch      = "search_batch"
	reliefBoundedQuery     = "bounded_query"

	reliefOutcomeGranted     = "granted"
	reliefOutcomeCapExceeded = "cap_exceeded"
	reliefOutcomeSecret      = "secret"
	reliefOutcomeTooLong     = "too_long"
	reliefOutcomeOperator    = "operator"
	reliefOutcomeUnattended  = "unattended"
	reliefOutcomeTaintedJob  = "tainted_job"
)

type WebOnlyLimits struct {
	Fetches int
	Queries int
}

type TaintedLimits struct {
	Fetches int
	Queries int
}

type reliefResult struct {
	relieved          bool
	fromSessionRecord bool
	approvalState     string
}

type resultBatchGrant struct {
	Approved        bool             `json:"approved"`
	URLs            []string         `json:"urls,omitempty"`
	Remaining       int              `json:"remaining"`
	StartFetchCount int              `json:"start_fetch_count"`
	Failure         *recordedFailure `json:"failure,omitempty"`
}

type batchGrantStore struct {
	mu     sync.Mutex
	order  []string
	grants map[string]resultBatchGrant
}

func (s *batchGrantStore) get(turnID string) (resultBatchGrant, bool) {
	if turnID == "" {
		return resultBatchGrant{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.grants[turnID]
	return grant, ok
}

func (s *batchGrantStore) put(turnID string, grant resultBatchGrant) {
	if turnID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.grants == nil {
		s.grants = make(map[string]resultBatchGrant, trackedBatchTurns)
	}
	if _, exists := s.grants[turnID]; !exists {
		s.order = append(s.order, turnID)
		if len(s.order) > trackedBatchTurns {
			delete(s.grants, s.order[0])
			s.order = s.order[1:]
		}
	}
	s.grants[turnID] = grant
}

func (b *Broker) recordTaintRelief(ctx context.Context, toolName, relief, outcome string) {
	if b.observeTaintRelief != nil {
		b.observeTaintRelief(ctx, toolName, relief, outcome)
	}
}

func (b *Broker) recordTaintedTrustGrant(ctx context.Context, outcome string) {
	if b.observeTaintedTrustGrant != nil {
		b.observeTaintedTrustGrant(ctx, outcome)
	}
}

func (b *Broker) evaluateRelief(ctx context.Context, request *ToolRequest, definition *tools.Definition, decision approval.PolicyDecision) (reliefResult, error) {
	relief := definition.SinkRelief
	if !request.Tainted || relief == nil || !definition.IsExternalSink() {
		return reliefResult{}, nil
	}
	if request.WebOnlyRelief {
		if b.webOnlyRelieved(request, definition) {
			return reliefResult{relieved: true}, nil
		}
		return reliefResult{}, nil
	}
	if requestClass(request) != principal.ClassOwner {
		return reliefResult{}, nil
	}
	value, ok := reliefArgument(request.Arguments, relief.Argument)
	if !ok {
		return reliefResult{}, nil
	}
	switch relief.Kind {
	case tools.SinkReliefLinkProvenance:
		return b.evaluateLinkRelief(ctx, request, definition, decision, value)
	case tools.SinkReliefBoundedQuery:
		return b.evaluateQueryRelief(ctx, request, definition, relief, value), nil
	default:
		return reliefResult{}, nil
	}
}

func (b *Broker) webOnlyRelieved(request *ToolRequest, definition *tools.Definition) bool {
	relief := definition.SinkRelief
	if !request.WebOnlyRelief || !request.Tainted || relief == nil || !definition.IsExternalSink() {
		return false
	}
	if requestClass(request) != principal.ClassOwner || request.TaintMark.Source == turn.TaintSourceScheduleOrigin {
		return false
	}
	value, ok := reliefArgument(request.Arguments, relief.Argument)
	if !ok || containsSecret(value, b.secretValues()) {
		return false
	}
	switch relief.Kind {
	case tools.SinkReliefLinkProvenance:
		if !plainURL(value) {
			return false
		}
		return request.FetchCount >= 0 && request.FetchCount < b.webOnly.Fetches
	case tools.SinkReliefBoundedQuery:
		if len(value) > relief.MaxBytes {
			return false
		}
		return request.QueryCount >= 0 && request.QueryCount < b.webOnly.Queries
	default:
		return false
	}
}

func (b *Broker) evaluateLinkRelief(ctx context.Context, request *ToolRequest, definition *tools.Definition, decision approval.PolicyDecision, rawURL string) (reliefResult, error) {
	secrets := b.secretValues()
	reliefKind := provenanceMetricKind(request.LinkProvenance)
	if containsSecret(rawURL, secrets) {
		if reliefKind != "" {
			b.recordTaintRelief(ctx, request.ToolName, reliefKind, reliefOutcomeSecret)
		}
		return reliefResult{}, nil
	}
	if len(rawURL) > maxReliefURLBytes {
		if reliefKind != "" {
			b.recordTaintRelief(ctx, request.ToolName, reliefKind, reliefOutcomeTooLong)
		}
		return reliefResult{}, nil
	}
	if !plainURL(rawURL) {
		return reliefResult{}, nil
	}
	canonicalURL, ok := tools.CanonicalURL(rawURL)
	if !ok {
		return reliefResult{}, nil
	}
	isTaintedJob := request.TaintMark.Source == turn.TaintSourceScheduleOrigin
	switch request.LinkProvenance {
	case tools.LinkProvenanceFetched:
		if len(request.SessionRecord) == 0 {
			return reliefResult{}, nil
		}
		b.recordTaintRelief(ctx, request.ToolName, reliefLinkFetched, reliefOutcomeGranted)
		return reliefResult{relieved: true, fromSessionRecord: true}, nil
	case tools.LinkProvenanceOwnerMessage:
		if isTaintedJob {
			b.recordTaintRelief(ctx, request.ToolName, reliefLinkOwnerMessage, reliefOutcomeTaintedJob)
			return reliefResult{}, nil
		}
		if request.FetchCount < 0 || request.FetchCount >= b.tainted.Fetches {
			b.recordTaintRelief(ctx, request.ToolName, reliefLinkOwnerMessage, reliefOutcomeCapExceeded)
			return reliefResult{}, nil
		}
		b.recordTaintRelief(ctx, request.ToolName, reliefLinkOwnerMessage, reliefOutcomeGranted)
		return reliefResult{relieved: true}, nil
	case tools.LinkProvenanceSearchResult:
		return b.evaluateSearchBatch(ctx, request, definition, decision, canonicalURL, isTaintedJob)
	default:
		return reliefResult{}, nil
	}
}

func provenanceMetricKind(p tools.LinkProvenance) string {
	switch p {
	case tools.LinkProvenanceOwnerMessage:
		return reliefLinkOwnerMessage
	case tools.LinkProvenanceFetched:
		return reliefLinkFetched
	case tools.LinkProvenanceSearchResult:
		return reliefSearchBatch
	default:
		return ""
	}
}

func (b *Broker) evaluateSearchBatch(ctx context.Context, request *ToolRequest, definition *tools.Definition, decision approval.PolicyDecision, canonicalURL string, isTaintedJob bool) (reliefResult, error) {
	if isTaintedJob {
		b.recordTaintRelief(ctx, request.ToolName, reliefSearchBatch, reliefOutcomeTaintedJob)
		return reliefResult{}, nil
	}
	if unattendedRequest(request, definition) {
		b.recordTaintRelief(ctx, request.ToolName, reliefSearchBatch, reliefOutcomeUnattended)
		return reliefResult{}, nil
	}
	existing, hasExisting := b.batchGrants.get(request.TurnID)
	if hasExisting {
		if existing.Failure != nil {
			return reliefResult{approvalState: ApprovalRejected}, fault.New(existing.Failure.Class, string(redact([]byte(existing.Failure.Detail), b.secretValues())))
		}
		if !existing.Approved {
			return reliefResult{approvalState: ApprovalRejected}, errorf(ResultApprovalRequired, "tool %q search result batch approval was rejected", toolKey(request))
		}
		if !slices.Contains(existing.URLs, canonicalURL) {
			return reliefResult{}, nil
		}
		usedFromBatch := request.FetchCount - existing.StartFetchCount
		if request.FetchCount < 0 || request.FetchCount >= b.tainted.Fetches || usedFromBatch < 0 || usedFromBatch >= existing.Remaining {
			b.recordTaintRelief(ctx, request.ToolName, reliefSearchBatch, reliefOutcomeCapExceeded)
			return reliefResult{}, nil
		}
		b.recordTaintRelief(ctx, request.ToolName, reliefSearchBatch, reliefOutcomeGranted)
		return reliefResult{relieved: true, approvalState: ApprovalApproved}, nil
	}
	if request.FetchCount < 0 || request.FetchCount >= b.tainted.Fetches {
		b.recordTaintRelief(ctx, request.ToolName, reliefSearchBatch, reliefOutcomeCapExceeded)
		return reliefResult{}, nil
	}
	if request.ResultBatch == nil {
		return reliefResult{}, nil
	}
	candidates := batchURLSet(request.ResultBatch.URLs)
	if !slices.Contains(candidates, canonicalURL) {
		return reliefResult{}, nil
	}
	grant, err := b.resolveBatchGrant(ctx, request, decision, existing, hasExisting)
	if err != nil {
		return reliefResult{approvalState: ApprovalRejected}, err
	}
	if !grant.Approved {
		return reliefResult{approvalState: ApprovalRejected}, errorf(ResultApprovalRequired, "tool %q search result batch approval was rejected", toolKey(request))
	}
	if !slices.Contains(grant.URLs, canonicalURL) {
		return reliefResult{}, nil
	}
	usedFromBatch := request.FetchCount - grant.StartFetchCount
	if usedFromBatch < 0 || usedFromBatch >= grant.Remaining || request.FetchCount >= b.tainted.Fetches {
		b.recordTaintRelief(ctx, request.ToolName, reliefSearchBatch, reliefOutcomeCapExceeded)
		return reliefResult{}, nil
	}
	b.recordTaintRelief(ctx, request.ToolName, reliefSearchBatch, reliefOutcomeGranted)
	return reliefResult{relieved: true, approvalState: ApprovalApproved}, nil
}

func (b *Broker) resolveBatchGrant(ctx context.Context, request *ToolRequest, decision approval.PolicyDecision, existing resultBatchGrant, hasExisting bool) (resultBatchGrant, error) {
	scope, scoped := durable.TurnScopeFrom(ctx)
	if !scoped {
		if hasExisting {
			if existing.Failure != nil {
				return resultBatchGrant{}, fault.New(existing.Failure.Class, string(redact([]byte(existing.Failure.Detail), b.secretValues())))
			}
			return existing, nil
		}
		grant, err := b.askBatchCard(ctx, request, decision)
		if err != nil {
			return resultBatchGrant{}, err
		}
		b.batchGrants.put(request.TurnID, grant)
		if grant.Failure != nil {
			return resultBatchGrant{}, fault.New(grant.Failure.Class, string(redact([]byte(grant.Failure.Detail), b.secretValues())))
		}
		return grant, nil
	}
	raw, err := scope.Invocation().RunAction(ctx, resultBatchAction, func(ctx context.Context) ([]byte, error) {
		callCtx, cancel := withCallDeadline(ctx, request.Deadline)
		defer cancel()
		grant, askErr := b.askBatchCard(callCtx, request, decision)
		if askErr != nil {
			return nil, askErr
		}
		return json.Marshal(grant)
	})
	if err != nil {
		return resultBatchGrant{}, b.unrecordedAuthorization(request, err)
	}
	grant, ok := decodeBatchGrant(raw)
	if !ok {
		return resultBatchGrant{}, errorf(ResultPolicyDenied, "tool %q has an unreadable search result batch grant", toolKey(request))
	}
	b.batchGrants.put(request.TurnID, grant)
	if grant.Failure != nil {
		return resultBatchGrant{}, fault.New(grant.Failure.Class, string(redact([]byte(grant.Failure.Detail), b.secretValues())))
	}
	return grant, nil
}

func (b *Broker) askBatchCard(ctx context.Context, request *ToolRequest, decision approval.PolicyDecision) (resultBatchGrant, error) {
	if err := contextError(ctx, request.Deadline); err != nil {
		return resultBatchGrant{}, err
	}
	key := toolKey(request)
	if !b.sessionReachable(request.SessionID) {
		err := errorf(ResultUntrustedInputNoApprover, "tool %q needs approval because this turn carries untrusted input, and nobody can be asked", key)
		return recordedBatchFailure(err, b.secretValues()), nil
	}
	if b.decider == nil {
		err := errorf(ResultApprovalRequired, "tool %q requires approval", key)
		return recordedBatchFailure(err, b.secretValues()), nil
	}
	remaining := b.tainted.Fetches - request.FetchCount
	if remaining <= 0 {
		err := errorf(ResultApprovalRequired, "tool %q search result batch budget is exhausted", key)
		return recordedBatchFailure(err, b.secretValues()), nil
	}
	text, urls, err := renderBatchCardText(request.ToolName, remaining, request.ResultBatch, b.secretValues())
	if err != nil {
		return recordedBatchFailure(err, b.secretValues()), nil
	}
	prompt := &ApprovalPrompt{
		ToolName:             request.ToolName,
		ToolVersion:          request.ToolVersion,
		SessionID:            request.SessionID,
		TurnID:               request.TurnID,
		PrincipalID:          request.PrincipalID,
		Arguments:            text,
		Network:              decision.Constraints.AllowNetwork,
		Timeout:              decision.Constraints.Timeout,
		MaxOutputBytes:       decision.Constraints.MaxOutputBytes,
		PolicyVersion:        decision.PolicyVersion,
		ReasonCode:           approval.ReasonUntrustedInput,
		TaintSource:          taintSourceOf(request),
		ExpiresAt:            absoluteApprovalExpiry(request.Deadline),
		StandingGrantAllowed: false,
	}
	accepted, err := b.decider(ctx, prompt)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return resultBatchGrant{}, errorf(ResultDeadlineExceeded, "tool request ended: %v", err)
		}
		fail := errorf(ResultPolicyDenied, "tool %q approval failed closed: %s", key, redact([]byte(err.Error()), b.secretValues()))
		return recordedBatchFailure(fail, b.secretValues()), nil
	}
	if !accepted {
		return resultBatchGrant{Approved: false}, nil
	}
	return resultBatchGrant{
		Approved:        true,
		URLs:            urls,
		Remaining:       remaining,
		StartFetchCount: request.FetchCount,
	}, nil
}

func recordedBatchFailure(err error, secrets []string) resultBatchGrant {
	var coded *fault.Error[ResultClass]
	class := ResultPolicyDenied
	detail := err.Error()
	if errors.As(err, &coded) && recordableClass(coded.Code) {
		class = coded.Code
		detail = coded.Detail
	}
	return resultBatchGrant{
		Failure: &recordedFailure{
			Class:  class,
			Detail: string(redact([]byte(detail), secrets)),
		},
	}
}

func decodeBatchGrant(raw []byte) (resultBatchGrant, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var grant resultBatchGrant
	if err := decoder.Decode(&grant); err != nil {
		return resultBatchGrant{}, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return resultBatchGrant{}, false
	}
	if grant.Failure != nil && !recordableClass(grant.Failure.Class) {
		return resultBatchGrant{}, false
	}
	return grant, true
}

func batchURLSet(rawURLs []string) []string {
	urls := make([]string, 0, min(len(rawURLs), maxBatchURLs))
	seen := make(map[string]struct{}, min(len(rawURLs), maxBatchURLs))
	for _, raw := range rawURLs {
		canonical, ok := tools.CanonicalURL(raw)
		if !ok {
			continue
		}
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		urls = append(urls, canonical)
		if len(urls) >= maxBatchURLs {
			break
		}
	}
	return urls
}

func renderBatchCardText(toolName string, limit int, batch *tools.ResultBatch, secrets []string) (text string, urls []string, err error) {
	if batch == nil {
		return "", nil, errorf(ResultPolicyDenied, "tool %q has no search result batch", toolName)
	}
	urls = batchURLSet(batch.URLs)
	lines := make([]string, 0, 1+len(batch.Queries)+len(urls))
	lines = append(lines, fmt.Sprintf("Read up to %d of these search results?", limit))
	for _, query := range batch.Queries {
		if containsSecret(query, secrets) {
			return "", nil, errorf(ResultPolicyDenied, "tool %q search query holds a configured secret, so the approval card cannot show it", toolName)
		}
		lines = append(lines, "query: "+tools.EscapeDisplayText(query))
	}
	for _, u := range urls {
		if containsSecret(u, secrets) {
			return "", nil, errorf(ResultPolicyDenied, "tool %q search result holds a configured secret, so the approval card cannot show it", toolName)
		}
		lines = append(lines, "url: "+tools.EscapeDisplayText(u))
	}
	joined := strings.Join(lines, "\n")
	if len(joined) > tools.ApprovalDisplayLimit {
		return "", nil, errorf(ResultApprovalDisplayTooLarge, "tool %q search result batch does not fit the %d byte approval display", toolName, tools.ApprovalDisplayLimit)
	}
	return joined, urls, nil
}

func (b *Broker) evaluateQueryRelief(ctx context.Context, request *ToolRequest, definition *tools.Definition, relief *tools.SinkRelief, query string) reliefResult {
	if request.TaintMark.Source == turn.TaintSourceScheduleOrigin {
		b.recordTaintRelief(ctx, request.ToolName, reliefBoundedQuery, reliefOutcomeTaintedJob)
		return reliefResult{}
	}
	if unattendedRequest(request, definition) && !cleanJobRequest(request) {
		b.recordTaintRelief(ctx, request.ToolName, reliefBoundedQuery, reliefOutcomeUnattended)
		return reliefResult{}
	}
	if containsSecret(query, b.secretValues()) {
		b.recordTaintRelief(ctx, request.ToolName, reliefBoundedQuery, reliefOutcomeSecret)
		return reliefResult{}
	}
	if len(query) > relief.MaxBytes {
		b.recordTaintRelief(ctx, request.ToolName, reliefBoundedQuery, reliefOutcomeTooLong)
		return reliefResult{}
	}
	if tools.QueryHasURLPattern(query) || tools.QueryHasSearchOperator(query) {
		b.recordTaintRelief(ctx, request.ToolName, reliefBoundedQuery, reliefOutcomeOperator)
		return reliefResult{}
	}
	if request.QueryCount < 0 || request.QueryCount >= b.tainted.Queries {
		b.recordTaintRelief(ctx, request.ToolName, reliefBoundedQuery, reliefOutcomeCapExceeded)
		return reliefResult{}
	}
	b.recordTaintRelief(ctx, request.ToolName, reliefBoundedQuery, reliefOutcomeGranted)
	return reliefResult{relieved: true}
}

func cleanJobRequest(request *ToolRequest) bool {
	return request.Origin == turn.OriginSchedule &&
		request.TaintMark.Source != turn.TaintSourceScheduleOrigin &&
		request.TaintMark.Source != turn.TaintSourceWebOnly
}

func sessionRecordOutput(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errorf(ResultExecutionFailed, "recorded web_fetch output is not a json object")
	}
	fields["from_session_record"] = json.RawMessage("true")
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, errorf(ResultExecutionFailed, "encode recorded web_fetch output: %v", err)
	}
	return encoded, nil
}

func taintSourceOf(request *ToolRequest) string {
	if request == nil || !request.Tainted {
		return ""
	}
	if request.TaintSource != "" {
		return request.TaintSource
	}
	if request.TaintMark.Source != "" {
		return request.TaintMark.Label()
	}
	return turn.TaintMark{}.Label()
}

func reliefArgument(arguments json.RawMessage, name string) (string, bool) {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &decoded); err != nil {
		return "", false
	}
	var value string
	if err := json.Unmarshal(decoded[name], &value); err != nil || !utf8.ValidString(value) {
		return "", false
	}
	return value, true
}

func plainURL(raw string) bool {
	if len(raw) > maxReliefURLBytes {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Host == "" || strings.Contains(parsed.Host, "@") {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return scheme == "http" || scheme == "https"
}

func containsSecret(value string, secrets []string) bool {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(value, secret) {
			return true
		}
	}
	return false
}

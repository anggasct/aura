package profile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

type OperationDeps struct {
	Service *Service
	Now     func() time.Time
}

type Operations struct {
	service *Service
	now     func() time.Time
}

func NewOperations(deps *OperationDeps) (*Operations, error) {
	if deps == nil || deps.Service == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "profile operation deps need a service")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Operations{service: deps.Service, now: now}, nil
}

func UnavailableOperations() *Operations {
	return &Operations{now: time.Now}
}

func (o *Operations) Bindings() []operation.Binding {
	return []operation.Binding{
		{ID: operationAccept, Handle: o.accept, Preview: o.previewAccept},
		{ID: operationDelete, Handle: o.delete, Preview: o.previewDelete},
		{ID: operationList, Handle: o.list},
		{ID: operationReject, Handle: o.reject},
		{ID: operationReview, Handle: o.review},
		{ID: operationSet, Handle: o.set},
		{ID: operationShow, Handle: o.show},
	}
}

func moduleFailure(err error) error {
	return operation.ModuleFailure[ErrorCode](err)
}

func usage(detail string) error {
	return operation.NewError(operation.ErrorCodeInputInvalid, detail)
}

func staleSet(detail string) error {
	return operation.NotPerformed(operation.ErrorCodePreviewStale, operation.NewError(operation.ErrorCodePreviewStale, detail))
}

func (o *Operations) ready(call *operation.Call, into any) error {
	if o.service == nil {
		return fault.New(ErrorCodeInvalidArgument, "the profile service is not available in this command")
	}
	return call.Decode(into)
}

func textResult(lines ...string) (operation.Result, error) {
	return operation.JSONResult(map[string]any{"text": strings.Join(lines, "\n")})
}

func verifiedMark(verified bool) string {
	if verified {
		return " verified"
	}
	return ""
}

func factDigest(fact *Fact) string {
	if fact == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		fact.ID, fact.Category, fact.Key, fact.Status, fact.ValueDigest, fact.Origin, strconv.FormatBool(fact.OwnerVerified), fact.TaintSource,
	}, "|")))
	return hex.EncodeToString(sum[:])
}

type factRow struct {
	ID            string  `json:"id"`
	Category      string  `json:"category"`
	Key           string  `json:"key"`
	Value         string  `json:"value"`
	Status        string  `json:"status"`
	Confidence    float64 `json:"confidence"`
	Origin        string  `json:"origin"`
	Verified      bool    `json:"verified"`
	TaintSource   string  `json:"taint_source,omitempty"`
	ConflictsWith string  `json:"conflicts_with,omitempty"`
}

func rowOf(fact *Fact) factRow {
	if fact == nil {
		return factRow{}
	}
	return factRow{
		ID: fact.ID, Category: fact.Category, Key: fact.Key, Value: fact.Value, Status: fact.Status,
		Confidence: fact.Confidence, Origin: fact.Origin, Verified: fact.OwnerVerified,
		TaintSource: fact.TaintSource, ConflictsWith: fact.ConflictsWithID,
	}
}

type listInput struct {
	Category string `json:"category"`
	Status   string `json:"status"`
}

func (o *Operations) list(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in listInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	facts, err := o.service.ListFacts(ctx, principal.OwnerID, in.Status, in.Category, reviewedFactsCap)
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	rows := make([]factRow, 0, len(facts))
	for i := range facts {
		rows = append(rows, rowOf(&facts[i]))
	}
	return operation.JSONResult(map[string]any{"rows": rows})
}

type factInput struct {
	ID string `json:"id"`
}

type shownFact struct {
	factRow
	Created   string   `json:"created"`
	ExpiresAt string   `json:"expires_at,omitempty"`
	Evidence  []string `json:"evidence"`
}

func (o *Operations) show(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in factInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	fact, err := o.service.GetFact(ctx, principal.OwnerID, in.ID)
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	evidence, err := o.service.EvidenceFor(ctx, fact.ID)
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	shown := shownFact{factRow: rowOf(&fact), Created: fact.CreatedAt.Format(time.RFC3339), Evidence: make([]string, 0, len(evidence))}
	if fact.ExpiresAt != nil {
		shown.ExpiresAt = fact.ExpiresAt.Format(time.RFC3339)
	}
	for i := range evidence {
		item := &evidence[i]
		shown.Evidence = append(shown.Evidence, fmt.Sprintf("event=%s model=%s/%s prompt=%s",
			item.SourceEventID, item.Provider, item.Model, item.PromptVersion))
	}
	return operation.JSONResult(shown)
}

type reviewRow struct {
	State         string  `json:"state"`
	ID            string  `json:"id"`
	Category      string  `json:"category"`
	Key           string  `json:"key"`
	Value         string  `json:"value"`
	Origin        string  `json:"origin"`
	Confidence    float64 `json:"confidence"`
	ConflictsWith string  `json:"conflicts_with,omitempty"`
	TaintSource   string  `json:"taint_source,omitempty"`
}

func reviewRowOf(state string, fact *Fact) reviewRow {
	if fact == nil {
		return reviewRow{}
	}
	return reviewRow{
		State: state, ID: fact.ID, Category: fact.Category, Key: fact.Key, Value: fact.Value,
		Origin: fact.Origin, Confidence: fact.Confidence, ConflictsWith: fact.ConflictsWithID, TaintSource: fact.TaintSource,
	}
}

func (o *Operations) review(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in struct{}
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	candidates, err := o.service.ListFacts(ctx, principal.OwnerID, StatusCandidate, "", reviewedFactsCap)
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	actives, err := o.service.ListFacts(ctx, principal.OwnerID, StatusActive, "", reviewedFactsCap)
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	var conflicted, plain, unverified []reviewRow
	for i := range candidates {
		fact := &candidates[i]
		if fact.ConflictsWithID != "" {
			conflicted = append(conflicted, reviewRowOf("conflict", fact))
		} else {
			plain = append(plain, reviewRowOf("candidate", fact))
		}
	}
	for i := range actives {
		if !actives[i].OwnerVerified {
			unverified = append(unverified, reviewRowOf("unverified", &actives[i]))
		}
	}
	rows := append(append(conflicted, plain...), unverified...)
	if rows == nil {
		rows = []reviewRow{}
	}
	return operation.JSONResult(map[string]any{"rows": rows})
}

func acceptPreviewLines(fact *Fact) []string {
	if fact == nil {
		return nil
	}
	return []string{
		"Fact: " + fact.ID,
		"Category: " + fact.Category,
		"Key: " + fact.Key,
		"Value: " + fact.Value,
		"Origin: " + fact.Origin,
		"Taint source: " + fact.TaintSource,
		"Accepting marks this fact owner-verified for every owner turn and clears its taint source.",
	}
}

func (o *Operations) previewAccept(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	var in factInput
	if err := o.ready(call, &in); err != nil {
		return operation.Preview{}, err
	}
	fact, err := o.service.GetFact(ctx, principal.OwnerID, in.ID)
	if err != nil {
		return operation.Preview{}, moduleFailure(err)
	}
	if fact.Status == StatusDeleted {
		return operation.Preview{}, moduleFailure(fault.New(ErrorCodeProfileConflict, "deleted facts cannot be accepted"))
	}
	if fact.TaintSource == "" {
		return operation.Preview{Digest: factDigest(&fact), Direct: true}, nil
	}
	return operation.Preview{Lines: acceptPreviewLines(&fact), Digest: factDigest(&fact)}, nil
}

func (o *Operations) accept(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in factInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	var committedFact Fact
	var hasCommitted bool
	result, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		current, err := o.service.GetFact(ctx, principal.OwnerID, in.ID)
		if err != nil {
			return operation.Result{}, err
		}
		if current.TaintSource != "" {
			if call.Confirmation == "" {
				return operation.Result{}, operation.NewError(operation.ErrorCodeConfirmation, "accepting a tainted fact requires the preview confirmation code")
			}
			if call.PreviewDigest == "" || factDigest(&current) != call.PreviewDigest {
				return operation.Result{}, staleSet("the fact changed after the preview")
			}
		}
		fact, err := o.service.AcceptFact(ctx, principal.OwnerID, in.ID, o.now().UTC())
		if err != nil {
			return operation.Result{}, err
		}
		committedFact = fact
		hasCommitted = true
		return operation.JSONResult(map[string]any{
			"accepted": fact.ID, "category": fact.Category, "key": fact.Key, "value": fact.Value, "origin": fact.Origin,
		})
	})
	if err != nil {
		return result, moduleFailure(err)
	}
	if hasCommitted {
		o.service.EmbedFactBestEffort(ctx, &committedFact)
	}
	return result, nil
}

func (o *Operations) reject(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in factInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	result, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		if err := o.service.RejectFact(ctx, principal.OwnerID, in.ID, o.now().UTC()); err != nil {
			return operation.Result{}, err
		}
		return textResult("rejected: " + in.ID)
	})
	return result, moduleFailure(err)
}

type setInput struct {
	Category  string `json:"category"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	ExpiresAt string `json:"expires_at"`
}

func (o *Operations) futureTime(text string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, usage("expires_at must be RFC3339")
	}
	if !parsed.After(o.now().UTC()) {
		return time.Time{}, usage("expires_at must be in the future")
	}
	return parsed, nil
}

type setRequest struct {
	category    string
	key         string
	value       string
	taintSource string
	expires     *time.Time
}

const unknownTaintSourceLabel = "content from outside your control"

func callerTaintSource(caller *operation.Caller) string {
	if caller == nil || !caller.Tainted {
		return ""
	}
	source := strings.TrimSpace(caller.TaintSource)
	if source == "" {
		return unknownTaintSourceLabel
	}
	if len(source) > turn.MaxTaintDetailBytes {
		cut := turn.MaxTaintDetailBytes
		for cut > 0 && !utf8.RuneStart(source[cut]) {
			cut--
		}
		source = strings.ToValidUTF8(source[:cut], "")
	}
	return source
}

func (o *Operations) set(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in setInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	request := &setRequest{category: in.Category, key: in.Key, value: in.Value}
	if in.ExpiresAt != "" {
		expires, err := o.futureTime(in.ExpiresAt)
		if err != nil {
			return operation.Result{}, err
		}
		request.expires = &expires
	}
	var committedFact Fact
	var hasCommitted bool
	var apply func(ctx context.Context) (operation.Result, error)
	switch call.Caller.Surface {
	case operation.SurfaceCLI, operation.SurfaceChat:
		apply = func(ctx context.Context) (operation.Result, error) {
			fact, err := o.service.SetFact(ctx, principal.OwnerID, request.category, request.key, request.value, request.expires, o.now().UTC())
			if err != nil {
				return operation.Result{}, err
			}
			committedFact = fact
			hasCommitted = true
			return textResult("set: " + fact.ID)
		}
	case operation.SurfaceAgent:
		request.taintSource = callerTaintSource(&call.Caller)
		apply = func(ctx context.Context) (operation.Result, error) { return o.proposeUnverified(ctx, request) }
	default:
		return operation.Result{}, operation.NewError(operation.ErrorCodeNotExposed, "operation: this surface cannot set a fact")
	}
	result, err := call.Commit(ctx, apply)
	if err != nil {
		return result, moduleFailure(err)
	}
	if hasCommitted {
		o.service.EmbedFactBestEffort(ctx, &committedFact)
	}
	return result, nil
}

func (o *Operations) proposeUnverified(ctx context.Context, request *setRequest) (operation.Result, error) {
	fact, err := o.service.ProposeAgentFact(ctx, &AgentProposal{
		OwnerID: principal.OwnerID, Category: request.category, Key: request.key, Value: request.value,
		TaintSource: request.taintSource, ExpiresAt: request.expires,
	}, o.now().UTC())
	if err != nil {
		return operation.Result{}, err
	}
	return textResult(proposalOutcome(&fact))
}

func proposalOutcome(fact *Fact) string {
	if fact == nil {
		return ""
	}
	switch {
	case fact.Status == StatusActive && fact.OwnerVerified:
		return "stored: " + fact.ID + " (owner-verified, unchanged)"
	case fact.Status == StatusActive:
		return "stored: " + fact.ID + " (active, not verified by the owner)"
	}
	return "proposed: " + fact.ID + " (unverified, waits for the owner's review)"
}

func cardLines(fact *Fact) []string {
	if fact == nil {
		return nil
	}
	return []string{
		"would delete 1 profile fact",
		fmt.Sprintf("%s %s/%s = %s origin=%s%s", fact.ID, fact.Category, turn.QuoteUntrustedName(fact.Key), turn.QuoteUntrustedName(fact.Value), fact.Origin, verifiedMark(fact.OwnerVerified)),
		"this cannot be undone",
	}
}

func ownerDeleteLines(fact *Fact) []string {
	if fact == nil {
		return nil
	}
	return []string{
		"dry-run: would delete",
		fmt.Sprintf("%s %s/%s=%s status=%s conf=%.2f origin=%s%s", fact.ID, fact.Category, fact.Key, fact.Value, fact.Status, fact.Confidence, fact.Origin, verifiedMark(fact.OwnerVerified)),
	}
}

func (o *Operations) previewDelete(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	var in factInput
	if err := o.ready(call, &in); err != nil {
		return operation.Preview{}, err
	}
	fact, err := o.service.GetFact(ctx, principal.OwnerID, in.ID)
	if err != nil {
		return operation.Preview{}, moduleFailure(err)
	}
	if fact.Status == StatusDeleted {
		return operation.Preview{Lines: []string{"nothing to delete"}, Digest: factDigest(&fact), Empty: true}, nil
	}
	lines := cardLines(&fact)
	if surface := call.Caller.Surface; surface == operation.SurfaceCLI || surface == operation.SurfaceChat {
		lines = ownerDeleteLines(&fact)
	}
	return operation.Preview{Lines: lines, Digest: factDigest(&fact)}, nil
}

func (o *Operations) delete(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in factInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	if call.PreviewDigest == "" {
		return operation.Result{}, staleSet("no preview binds this call")
	}
	result, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		fact, err := o.service.GetFact(ctx, principal.OwnerID, in.ID)
		if err != nil {
			return operation.Result{}, err
		}
		if factDigest(&fact) != call.PreviewDigest {
			return operation.Result{}, staleSet("the fact changed after the preview")
		}
		if err := o.service.DeleteFact(ctx, principal.OwnerID, in.ID, o.now().UTC()); err != nil {
			return operation.Result{}, err
		}
		return textResult("deleted: " + in.ID)
	})
	return result, moduleFailure(err)
}

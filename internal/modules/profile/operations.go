package profile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	listedValueRunes = 40
	shownValueRunes  = 200
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
		{ID: operationAccept, Handle: o.accept},
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

func truncateText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

func verifiedMark(verified bool) string {
	if verified {
		return " verified"
	}
	return ""
}

func conflictMark(conflictsWith string) string {
	if conflictsWith != "" {
		return " conflicts-with=" + conflictsWith
	}
	return ""
}

func factLine(fact *Fact) string {
	return fmt.Sprintf("%s %s/%s=%s status=%s conf=%.2f origin=%s%s",
		fact.ID, fact.Category, fact.Key, truncateText(fact.Value, listedValueRunes), fact.Status,
		fact.Confidence, fact.Origin, verifiedMark(fact.OwnerVerified))
}

func factDigest(fact *Fact) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		fact.ID, fact.Status, fact.ValueDigest, fact.Origin, fmt.Sprint(fact.OwnerVerified),
	}, "|")))
	return hex.EncodeToString(sum[:])
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
	if len(facts) == 0 {
		return textResult("no facts")
	}
	lines := make([]string, 0, len(facts))
	for i := range facts {
		lines = append(lines, factLine(&facts[i]))
	}
	return textResult(lines...)
}

type factInput struct {
	ID string `json:"id"`
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
	lines := []string{
		"id: " + fact.ID,
		"category: " + fact.Category,
		"key: " + fact.Key,
		"value: " + truncateText(fact.Value, shownValueRunes),
		fmt.Sprintf("status: %s origin: %s%s conf: %.2f", fact.Status, fact.Origin, verifiedMark(fact.OwnerVerified), fact.Confidence),
		"created: " + fact.CreatedAt.Format(time.RFC3339),
	}
	if fact.ConflictsWithID != "" {
		lines = append(lines, "conflicts-with: "+fact.ConflictsWithID)
	}
	if fact.ExpiresAt != nil {
		lines = append(lines, "expires-at: "+fact.ExpiresAt.Format(time.RFC3339))
	}
	lines = append(lines, fmt.Sprintf("evidence: %d", len(evidence)))
	for i := range evidence {
		item := &evidence[i]
		lines = append(lines, fmt.Sprintf("  - event=%s model=%s/%s prompt=%s",
			item.SourceEventID, item.Provider, item.Model, item.PromptVersion))
	}
	return textResult(lines...)
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
	var conflicted, plain, unverified []string
	for i := range candidates {
		fact := &candidates[i]
		line := fmt.Sprintf("%s %s/%s=%s conf=%.2f%s",
			fact.ID, fact.Category, fact.Key, truncateText(fact.Value, listedValueRunes), fact.Confidence, conflictMark(fact.ConflictsWithID))
		if fact.ConflictsWithID != "" {
			conflicted = append(conflicted, line)
		} else {
			plain = append(plain, line)
		}
	}
	for i := range actives {
		fact := &actives[i]
		if fact.OwnerVerified {
			continue
		}
		unverified = append(unverified, fmt.Sprintf("%s %s/%s=%s conf=%.2f unverified%s",
			fact.ID, fact.Category, fact.Key, truncateText(fact.Value, listedValueRunes), fact.Confidence, conflictMark(fact.ConflictsWithID)))
	}
	if len(conflicted)+len(plain)+len(unverified) == 0 {
		return textResult("no candidates")
	}
	lines := append(append(conflicted, plain...), unverified...)
	return textResult(lines...)
}

func (o *Operations) accept(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in factInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	result, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		fact, err := o.service.AcceptFact(ctx, principal.OwnerID, in.ID, o.now().UTC())
		if err != nil {
			return operation.Result{}, err
		}
		return textResult("accepted: " + fact.ID)
	})
	return result, moduleFailure(err)
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

func (o *Operations) expiry(in *setInput) (*time.Time, error) {
	if in.ExpiresAt == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, in.ExpiresAt)
	if err != nil {
		return nil, usage("expires_at must be RFC3339")
	}
	if !parsed.After(o.now().UTC()) {
		return nil, usage("expires_at must be in the future")
	}
	return &parsed, nil
}

func (o *Operations) set(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in setInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	expires, err := o.expiry(&in)
	if err != nil {
		return operation.Result{}, err
	}
	switch call.Caller.Surface {
	case operation.SurfaceCLI, operation.SurfaceChat:
		return o.setVerified(ctx, call, &in, expires)
	case operation.SurfaceAgent:
		return o.proposeUnverified(ctx, call, &in, expires)
	}
	return operation.Result{}, operation.NewError(operation.ErrorCodeNotExposed, "operation: this surface cannot set a fact")
}

func (o *Operations) setVerified(ctx context.Context, call *operation.Call, in *setInput, expires *time.Time) (operation.Result, error) {
	result, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		fact, err := o.service.SetFact(ctx, principal.OwnerID, in.Category, in.Key, in.Value, expires, o.now().UTC())
		if err != nil {
			return operation.Result{}, err
		}
		return textResult("set: " + fact.ID)
	})
	return result, moduleFailure(err)
}

func (o *Operations) proposeUnverified(ctx context.Context, call *operation.Call, in *setInput, expires *time.Time) (operation.Result, error) {
	result, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		fact, err := o.service.ProposeAgentFact(ctx, &AgentProposal{
			OwnerID: principal.OwnerID, Category: in.Category, Key: in.Key, Value: in.Value, ExpiresAt: expires,
		}, o.now().UTC())
		if err != nil {
			return operation.Result{}, err
		}
		return textResult("proposed: " + fact.ID + " (unverified, waits for the owner's review)")
	})
	return result, moduleFailure(err)
}

func cardLines(fact *Fact) []string {
	return []string{
		"would delete 1 profile fact",
		fmt.Sprintf("%s %s/%s = %s", fact.ID, fact.Category, turn.QuoteUntrustedName(truncateText(fact.Key, listedValueRunes)), turn.QuoteUntrustedName(truncateText(fact.Value, listedValueRunes))),
		"this cannot be undone",
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
	lines := cardLines(&fact)
	if surface := call.Caller.Surface; surface == operation.SurfaceCLI || surface == operation.SurfaceChat {
		lines = []string{"dry-run: would delete", factLine(&fact)}
	}
	return operation.Preview{Lines: lines, Digest: factDigest(&fact), Empty: fact.Status == StatusDeleted}, nil
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

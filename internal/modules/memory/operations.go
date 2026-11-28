package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	previewLabelCap     = 20
	inspectContentRunes = 120
	nothingToDelete     = "nothing to delete"
)

type OperationDeps struct {
	Service    *Service
	Scopes     RecallScopes
	TaintMarks TaintMarksReader
	Logger     *slog.Logger
	Now        func() time.Time
}

type Operations struct {
	service    *Service
	scopes     RecallScopes
	taintMarks TaintMarksReader
	logger     *slog.Logger
	now        func() time.Time
}

func NewOperations(deps *OperationDeps) (*Operations, error) {
	if deps == nil || deps.Service == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "memory operation deps need a service")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Operations{service: deps.Service, scopes: deps.Scopes, taintMarks: deps.TaintMarks, logger: logger, now: now}, nil
}

func UnavailableOperations() *Operations {
	return &Operations{logger: slog.New(slog.DiscardHandler), now: time.Now}
}

func (o *Operations) Bindings() []operation.Binding {
	return []operation.Binding{
		{ID: operationInspect, Handle: o.inspect},
		{ID: operationForget, Handle: o.forget, Preview: o.previewForget},
		{ID: operationPurge, Handle: o.purge, Preview: o.previewPurge},
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
		return fault.New(ErrorCodeInvalidArgument, "the memory service is not available in this command")
	}
	if call == nil {
		return fault.New(ErrorCodeInvalidArgument, "call must not be nil")
	}
	return call.Decode(into)
}

func (o *Operations) readable(call *operation.Call) ([]string, error) {
	if call.Caller.Surface != operation.SurfaceAgent {
		return nil, nil
	}
	if o.scopes == nil {
		return nil, fault.New(ErrorCodeUnavailable, "the persona memory scopes are not available")
	}
	readable, known := o.scopes.RecallScope(call.Caller.AgentID)
	if !known || len(readable) == 0 {
		return nil, fault.New(ErrorCodeQueryInvalid, "the calling persona is not registered")
	}
	return readable, nil
}

func oneLine(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, text)
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

func expiresText(expires *time.Time) string {
	if expires == nil {
		return "never"
	}
	return expires.UTC().Format(time.RFC3339)
}

func promptText(version string) string {
	if version == "" {
		return "-"
	}
	return version
}

func documentLine(document *StoredDocument) string {
	if document == nil {
		return ""
	}
	return fmt.Sprintf("%s agent=%s session=%s kind=%s range=%d-%d trust=%s prompt=%s expires=%s content=%s",
		document.ID, document.AgentID, document.SessionID, document.Kind,
		document.FromSequence, document.ToSequence,
		document.TrustLabel, promptText(document.PromptVersion), expiresText(document.ExpiresAt),
		oneLine(truncateRunes(document.Content, inspectContentRunes)))
}

func documentsDigest(documents []StoredDocument) string {
	parts := make([]string, 0, len(documents))
	for i := range documents {
		content := sha256.Sum256([]byte(documents[i].Content))
		parts = append(parts, strings.Join([]string{documents[i].ID, hex.EncodeToString(content[:]), expiresText(documents[i].ExpiresAt)}, "|"))
	}
	slices.Sort(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func cardLabel(document *StoredDocument) string {
	if document == nil {
		return ""
	}
	return fmt.Sprintf("%s session=%s persona=%s %s %d-%d", document.ID, document.SessionID, document.AgentID, document.Kind, document.FromSequence, document.ToSequence)
}

func cardLines(headline string, documents []StoredDocument) []string {
	lines := []string{headline}
	shown := min(len(documents), previewLabelCap)
	for i := range shown {
		lines = append(lines, cardLabel(&documents[i]))
	}
	if more := len(documents) - shown; more > 0 {
		lines = append(lines, fmt.Sprintf("and %d more", more))
	}
	return append(lines, "this cannot be undone")
}

func ownerLines(documents []StoredDocument) []string {
	lines := make([]string, 0, 1+len(documents))
	lines = append(lines, fmt.Sprintf("dry-run: %d would be deleted", len(documents)))
	for i := range documents {
		lines = append(lines, documentLine(&documents[i]))
	}
	return lines
}

func deletionPreview(surface operation.Surface, headline string, documents []StoredDocument) operation.Preview {
	if len(documents) == 0 {
		return operation.Preview{Lines: []string{nothingToDelete}, Digest: documentsDigest(documents), Empty: true}
	}
	lines := ownerLines(documents)
	if surface == operation.SurfaceAgent {
		lines = cardLines(headline, documents)
	}
	return operation.Preview{Lines: lines, Digest: documentsDigest(documents)}
}

type inspectInput struct {
	Session string `json:"session"`
	Query   string `json:"query"`
	Limit   int    `json:"limit"`
}

type inspectRow struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Kind    string `json:"kind"`
	From    int64  `json:"from"`
	To      int64  `json:"to"`
	Trust   string `json:"trust"`
	Tainted bool   `json:"tainted"`
	Prompt  string `json:"prompt"`
	Expires string `json:"expires"`
	Content string `json:"content"`
}

func (o *Operations) inspect(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in inspectInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	readable, err := o.readable(call)
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	documents, err := o.service.Inspect(ctx, &DocumentQuery{
		OwnerID: principal.OwnerID, AgentIDs: readable, SessionID: in.Session, Terms: in.Query, Limit: in.Limit,
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	taintedByIndex := o.documentTaint(ctx, documents)
	rows := make([]inspectRow, 0, len(documents))
	for i := range documents {
		document := &documents[i]
		rows = append(rows, inspectRow{
			ID: document.ID, Agent: document.AgentID, Session: document.SessionID, Kind: document.Kind,
			From: document.FromSequence, To: document.ToSequence, Trust: document.TrustLabel, Tainted: taintedByIndex[i],
			Prompt: promptText(document.PromptVersion), Expires: expiresText(document.ExpiresAt),
			Content: oneLine(truncateRunes(document.Content, inspectContentRunes)),
		})
	}
	return operation.JSONResult(map[string]any{"rows": rows})
}

func (o *Operations) documentTaint(ctx context.Context, documents []StoredDocument) []bool {
	tainted := make([]bool, len(documents))
	if len(documents) == 0 {
		return tainted
	}
	if o.taintMarks == nil {
		for i := range tainted {
			tainted[i] = true
		}
		return tainted
	}
	seen := map[string]struct{}{}
	sessionIDs := make([]string, 0, len(documents))
	for i := range documents {
		sid := documents[i].SessionID
		if sid == "" {
			continue
		}
		if _, ok := seen[sid]; !ok {
			seen[sid] = struct{}{}
			sessionIDs = append(sessionIDs, sid)
		}
	}
	marks, err := o.taintMarks.SessionMarks(ctx, sessionIDs)
	if err != nil {
		for i := range tainted {
			tainted[i] = true
		}
		return tainted
	}
	for i := range documents {
		firstSeq, marked := marks[documents[i].SessionID]
		if marked && documents[i].ToSequence >= 0 && firstSeq <= uint64(documents[i].ToSequence) {
			tainted[i] = true
		}
	}
	return tainted
}

type forgetInput struct {
	Session string `json:"session"`
	ID      string `json:"id"`
	Query   string `json:"query"`
	Limit   int    `json:"limit"`
}

func (in *forgetInput) scope() (string, error) {
	if in == nil {
		return "", usage("forget requires exactly one of session, id, query")
	}
	var scopes []string
	if in.Session != "" {
		scopes = append(scopes, "session")
	}
	if in.ID != "" {
		scopes = append(scopes, "id")
	}
	if in.Query != "" {
		scopes = append(scopes, "query")
	}
	if len(scopes) != 1 {
		return "", usage("forget requires exactly one of session, id, query")
	}
	return scopes[0], nil
}

type forgetRequest struct {
	input    forgetInput
	scope    string
	readable []string
}

func (o *Operations) forgetRequestOf(call *operation.Call) (*forgetRequest, error) {
	request := &forgetRequest{}
	if err := o.ready(call, &request.input); err != nil {
		return nil, err
	}
	scope, err := request.input.scope()
	if err != nil {
		return nil, err
	}
	request.scope = scope
	if request.readable, err = o.readable(call); err != nil {
		return nil, moduleFailure(err)
	}
	return request, nil
}

func (o *Operations) doomed(ctx context.Context, request *forgetRequest) ([]StoredDocument, error) {
	if request == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "forget request must not be nil")
	}
	query := &DocumentQuery{
		OwnerID: principal.OwnerID, AgentIDs: request.readable, SessionID: request.input.Session,
		DocumentID: request.input.ID, Terms: request.input.Query, Limit: request.input.Limit,
	}
	switch request.scope {
	case "session":
		return o.service.PreviewForgetBySession(ctx, query)
	case "id":
		return o.service.PreviewForgetByID(ctx, query)
	}
	query.SessionID = ""
	return o.service.PreviewForgetByQuery(ctx, query)
}

func forgetHeadline(request *forgetRequest, count int) string {
	if request == nil {
		return ""
	}
	headline := fmt.Sprintf("would delete %d memory documents", count)
	switch request.scope {
	case "query":
		return headline + " matching " + turn.QuoteUntrustedName(request.input.Query)
	case "session":
		return headline + " of session " + turn.QuoteUntrustedName(request.input.Session)
	}
	return headline
}

func (o *Operations) previewForget(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	request, err := o.forgetRequestOf(call)
	if err != nil {
		return operation.Preview{}, err
	}
	documents, err := o.doomed(ctx, request)
	if err != nil {
		return operation.Preview{}, moduleFailure(err)
	}
	return deletionPreview(call.Caller.Surface, forgetHeadline(request, len(documents)), documents), nil
}

func requirePreview(call *operation.Call) error {
	if call.PreviewDigest == "" {
		return staleSet("no preview binds this call")
	}
	return nil
}

func (o *Operations) forget(ctx context.Context, call *operation.Call) (operation.Result, error) {
	request, err := o.forgetRequestOf(call)
	if err != nil {
		return operation.Result{}, err
	}
	if err := requirePreview(call); err != nil {
		return operation.Result{}, err
	}
	var applied bool
	var deleted int
	committed, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		documents, err := o.doomed(ctx, request)
		if err != nil {
			return operation.Result{}, err
		}
		if documentsDigest(documents) != call.PreviewDigest {
			return operation.Result{}, staleSet("the documents changed after the preview")
		}
		count, err := o.service.Forget(ctx, principal.OwnerID, documents)
		if err != nil {
			return operation.Result{}, err
		}
		if count != len(documents) {
			return operation.Result{}, fault.New(ErrorCodeMemoryInvalid, "the delete did not remove the previewed documents")
		}
		deleted, applied = count, true
		return operation.JSONResult(map[string]any{"text": "deleted: " + strconv.Itoa(count), "deleted": count})
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if applied {
		o.logger.InfoContext(ctx, "memory forget applied", "component", "memory", "operation", "forget", "scope", request.scope, "deleted", deleted)
	}
	return committed, nil
}

type purgeInput struct {
	Limit int `json:"limit"`
}

func (o *Operations) previewPurge(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	var in purgeInput
	if err := o.ready(call, &in); err != nil {
		return operation.Preview{}, err
	}
	documents, err := o.service.PreviewPurge(ctx, o.now(), in.Limit)
	if err != nil {
		return operation.Preview{}, moduleFailure(err)
	}
	headline := fmt.Sprintf("would delete %d expired summary documents", len(documents))
	return deletionPreview(call.Caller.Surface, headline, documents), nil
}

func (o *Operations) purge(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in purgeInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	if err := requirePreview(call); err != nil {
		return operation.Result{}, err
	}
	var applied bool
	var pruned int
	committed, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		now := o.now()
		documents, err := o.service.PreviewPurge(ctx, now, in.Limit)
		if err != nil {
			return operation.Result{}, err
		}
		if documentsDigest(documents) != call.PreviewDigest {
			return operation.Result{}, staleSet("the expired documents changed after the preview")
		}
		count, err := o.service.PurgeExpired(ctx, now, in.Limit)
		if err != nil {
			return operation.Result{}, err
		}
		if count != len(documents) {
			return operation.Result{}, fault.New(ErrorCodeMemoryInvalid, "the purge did not remove the previewed documents")
		}
		pruned, applied = count, true
		return operation.JSONResult(map[string]any{"text": "purged: " + strconv.Itoa(count), "purged": count})
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if applied {
		o.logger.InfoContext(ctx, "memory purge complete", "component", "memory", "operation", "purge", "pruned", pruned)
	}
	return committed, nil
}

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
)

const (
	previewLabelCap     = 10
	inspectContentRunes = 120
)

type OperationDeps struct {
	Service *Service
	Logger  *slog.Logger
	Now     func() time.Time
}

type Operations struct {
	service *Service
	logger  *slog.Logger
	now     func() time.Time
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
	return &Operations{service: deps.Service, logger: logger, now: now}, nil
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
	return call.Decode(into)
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

func documentLine(document *StoredDocument) string {
	expires := "never"
	if document.ExpiresAt != nil {
		expires = document.ExpiresAt.UTC().Format(time.RFC3339)
	}
	prompt := document.PromptVersion
	if prompt == "" {
		prompt = "-"
	}
	return fmt.Sprintf("%s agent=%s session=%s kind=%s range=%d-%d trust=%s prompt=%s expires=%s content=%s",
		document.ID, document.AgentID, document.SessionID, document.Kind,
		document.FromSequence, document.ToSequence,
		document.TrustLabel, prompt, expires,
		oneLine(truncateRunes(document.Content, inspectContentRunes)))
}

func documentLines(documents []StoredDocument) []string {
	if len(documents) == 0 {
		return []string{"no documents"}
	}
	lines := make([]string, 0, len(documents))
	for i := range documents {
		lines = append(lines, documentLine(&documents[i]))
	}
	return lines
}

func documentsDigest(documents []StoredDocument) string {
	parts := make([]string, 0, len(documents))
	for i := range documents {
		content := sha256.Sum256([]byte(documents[i].Content))
		expires := ""
		if documents[i].ExpiresAt != nil {
			expires = documents[i].ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		parts = append(parts, strings.Join([]string{documents[i].ID, hex.EncodeToString(content[:]), expires}, "|"))
	}
	slices.Sort(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func cardLines(headline string, documents []StoredDocument) []string {
	lines := []string{headline}
	shown := min(len(documents), previewLabelCap)
	for i := range shown {
		document := &documents[i]
		lines = append(lines, fmt.Sprintf("%s %s %d-%d", document.ID, document.Kind, document.FromSequence, document.ToSequence))
	}
	if more := len(documents) - shown; more > 0 {
		lines = append(lines, fmt.Sprintf("and %d more", more))
	}
	return append(lines, "this cannot be undone")
}

func previewLines(surface operation.Surface, noun string, documents []StoredDocument) []string {
	if surface == operation.SurfaceCLI || surface == operation.SurfaceChat {
		return append([]string{fmt.Sprintf("dry-run: %d would be deleted", len(documents))}, documentLines(documents)...)
	}
	return cardLines(fmt.Sprintf("would delete %d %s", len(documents), noun), documents)
}

type inspectInput struct {
	Session string `json:"session"`
	Query   string `json:"query"`
	Limit   int    `json:"limit"`
}

func (o *Operations) inspect(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in inspectInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	documents, err := o.service.Inspect(ctx, principal.OwnerID, in.Session, in.Query, in.Limit)
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	return operation.JSONResult(map[string]any{"text": strings.Join(documentLines(documents), "\n")})
}

type forgetInput struct {
	Session string `json:"session"`
	ID      string `json:"id"`
	Query   string `json:"query"`
	Limit   int    `json:"limit"`
}

func (in *forgetInput) scope() (string, error) {
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

func (o *Operations) forgetRequest(call *operation.Call) (in forgetInput, scope string, err error) {
	if err := o.ready(call, &in); err != nil {
		return in, "", err
	}
	scope, err = in.scope()
	return in, scope, err
}

func (o *Operations) doomed(ctx context.Context, in *forgetInput) ([]StoredDocument, error) {
	switch {
	case in.Session != "":
		return o.service.PreviewForgetBySession(ctx, principal.OwnerID, in.Session, in.Limit)
	case in.ID != "":
		return o.service.PreviewForgetByID(ctx, principal.OwnerID, in.ID)
	}
	return o.service.PreviewForgetByQuery(ctx, principal.OwnerID, "", in.Query, in.Limit)
}

func (o *Operations) previewForget(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	in, _, err := o.forgetRequest(call)
	if err != nil {
		return operation.Preview{}, err
	}
	documents, err := o.doomed(ctx, &in)
	if err != nil {
		return operation.Preview{}, moduleFailure(err)
	}
	return operation.Preview{
		Lines:  previewLines(call.Caller.Surface, "memory documents", documents),
		Digest: documentsDigest(documents),
		Empty:  len(documents) == 0,
	}, nil
}

func requirePreview(call *operation.Call) error {
	if call.PreviewDigest == "" {
		return staleSet("no preview binds this call")
	}
	return nil
}

func deletedResult(deleted int) (operation.Result, error) {
	return operation.JSONResult(map[string]any{"text": "deleted: " + strconv.Itoa(deleted), "deleted": deleted})
}

func (o *Operations) forget(ctx context.Context, call *operation.Call) (operation.Result, error) {
	in, scope, err := o.forgetRequest(call)
	if err != nil {
		return operation.Result{}, err
	}
	if err := requirePreview(call); err != nil {
		return operation.Result{}, err
	}
	var applied bool
	var deleted int
	committed, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		documents, err := o.doomed(ctx, &in)
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
		return deletedResult(count)
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if !applied {
		return committed, nil
	}
	o.logger.InfoContext(ctx, "memory forget applied", "component", "memory", "operation", "forget", "scope", scope, "deleted", deleted)
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
	return operation.Preview{
		Lines:  previewLines(call.Caller.Surface, "expired summary documents", documents),
		Digest: documentsDigest(documents),
		Empty:  len(documents) == 0,
	}, nil
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
	if !applied {
		return committed, nil
	}
	o.logger.InfoContext(ctx, "memory purge complete", "component", "memory", "operation", "purge", "pruned", pruned)
	return committed, nil
}

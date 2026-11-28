package knowledge

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	digestPrefixLength = 12
	workspaceScheme    = "workspace:"
	artifactScheme     = "artifact:"

	embeddingStateFailed = "failed"
	embeddingNotApplied  = ""

	maxTrustPreviewSources = 20
	trustWarningLine       = "Passages from this collection will no longer mark a conversation as carrying untrusted content; their text stays reference material, never instructions."
)

var removalNotes = []string{
	"not removed: earlier replies, recorded tool results, the durable runtime journal and older backups may still hold copies",
	"not removed: copies an embedding provider holds under its own terms, and vectors a running server keeps in memory until its next retrieval or restart",
	"raw database pages can keep fragments until the database is vacuumed",
}

type OperationDeps struct {
	Service *Service
	Logger  *slog.Logger
}

type Operations struct {
	service *Service
	logger  *slog.Logger
}

func NewOperations(deps *OperationDeps) (*Operations, error) {
	if deps == nil || deps.Service == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "knowledge operation deps need a service")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Operations{service: deps.Service, logger: logger}, nil
}

func UnavailableOperations() *Operations {
	return &Operations{logger: slog.New(slog.DiscardHandler)}
}

func (o *Operations) Bindings() []operation.Binding {
	return []operation.Binding{
		{ID: operationList, Handle: o.list},
		{ID: operationShow, Handle: o.show},
		{ID: operationCheck, Handle: o.check},
		{ID: operationAdd, Handle: o.add, Preview: o.previewAdd},
		{ID: operationRefresh, Handle: o.refresh},
		{ID: operationRemove, Handle: o.remove, Preview: o.previewRemove},
		{ID: operationEmbed, Handle: o.embed, Preview: o.previewEmbed},
		{ID: operationUnembed, Handle: o.unembed, Preview: o.previewUnembed},
		{ID: operationTrust, Handle: o.trust, Preview: o.previewTrust},
		{ID: operationUntrust, Handle: o.untrust},
	}
}

type collectionInput struct {
	Collection string `json:"collection"`
	Source     string `json:"source"`
	URL        string `json:"url"`
}

const sourceParam = "source"

func usage(detail string) error {
	return operation.NewError(operation.ErrorCodeInputInvalid, detail)
}

func moduleFailure(err error) error {
	return operation.ModuleFailure[ErrorCode](err)
}

func (o *Operations) ready(call *operation.Call, input *collectionInput) error {
	if o.service == nil {
		return Errorf(ErrorCodeInvalidArgument, "the knowledge service is not available in this command")
	}
	return call.Decode(input)
}

func linesResult(lines []string, data map[string]any) (operation.Result, error) {
	data["text"] = strings.Join(lines, "\n")
	return operation.JSONResult(data)
}

func formatOptionalRFC3339(t *time.Time) *string {
	if t == nil || t.IsZero() {
		return nil
	}
	formatted := t.UTC().Format(time.RFC3339)
	return &formatted
}

func trustState(value string) string {
	if value == TrustTrusted {
		return TrustTrusted
	}
	return TrustUntrusted
}

func (o *Operations) list(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	collections, err := o.service.List(ctx)
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	type row struct {
		Collection string  `json:"collection"`
		Sources    int     `json:"sources"`
		Chunks     int     `json:"chunks"`
		Generation int64   `json:"generation"`
		Trust      string  `json:"trust"`
		TrustedAt  *string `json:"trusted_at"`
	}
	rows := make([]row, 0, len(collections))
	for i := range collections {
		rows = append(rows, row{
			Collection: collections[i].ID,
			Sources:    collections[i].Sources,
			Chunks:     collections[i].Chunks,
			Generation: collections[i].Generation,
			Trust:      trustState(collections[i].Trust),
			TrustedAt:  formatOptionalRFC3339(collections[i].TrustedAt),
		})
	}
	return operation.JSONResult(map[string]any{"rows": rows})
}

func (o *Operations) show(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	detail, err := o.service.Show(ctx, in.Collection)
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	type listed struct {
		URI      string `json:"uri"`
		Label    string `json:"label"`
		Kind     string `json:"kind"`
		Chunks   int    `json:"chunks"`
		Digest   string `json:"digest"`
		Ingested string `json:"ingested"`
		Vouched  string `json:"vouched"`
	}
	sources := make([]listed, 0, len(detail.Sources))
	for i := range detail.Sources {
		source := &detail.Sources[i]
		vouched := "no"
		if detail.Trust == TrustTrusted && source.Vouched() {
			vouched = "yes"
		}
		sources = append(sources, listed{
			URI: source.URI, Label: source.Label, Kind: source.Kind, Chunks: source.Chunks,
			Digest:   source.ContentSHA256[:min(digestPrefixLength, len(source.ContentSHA256))],
			Ingested: source.IngestedAt.UTC().Format(time.RFC3339),
			Vouched:  vouched,
		})
	}
	return operation.JSONResult(map[string]any{
		"collection": detail.ID,
		"sources":    len(sources),
		"chunks":     detail.Chunks,
		"trust":      trustState(detail.Trust),
		"trusted_at": formatOptionalRFC3339(detail.TrustedAt),
		"rows":       sources,
	})
}

func (o *Operations) check(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	report, err := o.service.Check(ctx)
	if err != nil {
		if fault.HasCode(err, ErrorCodeIndexDamaged) {
			return operation.Result{}, moduleFailure(fault.New(ErrorCodeIndexDamaged,
				fmt.Sprintf("knowledge index check found %d problem(s): %s", len(report.Problems), strings.Join(report.Problems, "; "))))
		}
		return operation.Result{}, moduleFailure(err)
	}
	return operation.JSONResult(map[string]any{
		"collections": report.Collections, "sources": report.Sources, "chunks": report.Chunks,
		"indexed_chunks": report.IndexedChunks, "status": "ok",
	})
}

func (o *Operations) add(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	prepared, digest, err := o.prepareAdd(ctx, call, &in)
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if call.PreviewDigest != "" && call.PreviewDigest != digest {
		return operation.Result{}, staleSet("the source changed after the preview")
	}
	var written IngestResult
	var applied bool
	committed, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		result, err := o.service.Write(ctx, ActionAdd, prepared)
		if err != nil {
			return operation.Result{}, err
		}
		written, applied = result, true
		return ingestResult([]IngestResult{result}, nil, embeddedAs{})
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if !applied {
		return committed, nil
	}
	o.logger.InfoContext(ctx, "knowledge source ingested", sourceAttrs("add", &written)...)
	if written.Status == StatusUnchanged {
		return committed, nil
	}
	return ingestResult([]IngestResult{written}, nil, o.embedAfterWrite(ctx, in.Collection))
}

func (o *Operations) previewAdd(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Preview{}, err
	}
	if !call.HasFile(sourceParam) {
		line := addLine(in.Collection, nil)
		return operation.Preview{Lines: []string{line}, Digest: digestOf(line)}, nil
	}
	file, err := call.File(ctx, sourceParam)
	if err != nil {
		return operation.Preview{}, moduleFailure(err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, io.LimitReader(file, MaxSourceBytes+1)); err != nil {
		return operation.Preview{}, operation.NewError(operation.ErrorCodeSourceUnavailable, "the source cannot be read")
	}
	line := addLine(in.Collection, file)
	return operation.Preview{Lines: []string{line}, Digest: digestOf(line, hex.EncodeToString(hasher.Sum(nil)))}, nil
}

func addLine(collection string, file operation.File) string {
	if file == nil {
		return "fetches the address from this host and stores its text as searchable chunks in collection " + collection
	}
	return fmt.Sprintf("reads %s (%s, %d bytes) and stores its text as searchable chunks in collection %s",
		turn.QuoteUntrustedName(file.Name()), mediaTypeLabel(file.MediaType()), file.Size(), collection)
}

func mediaTypeLabel(mediaType string) string {
	if mediaType == "" {
		return "media type not recorded"
	}
	return mediaType
}

func sourceAttrs(verb string, result *IngestResult) []any {
	return []any{
		"component", "knowledge", "operation", verb, "collection", result.Collection,
		"status", result.Status, "chunks", result.Chunks,
	}
}

func (o *Operations) prepareAdd(ctx context.Context, call *operation.Call, in *collectionInput) (prepared *Prepared, digest string, err error) {
	switch {
	case in.URL != "":
		prepared, err = o.service.PrepareURL(ctx, in.Collection, in.URL)
		return prepared, digestOf(addLine(in.Collection, nil)), err
	case call.HasFile(sourceParam):
		file, err := call.File(ctx, sourceParam)
		if err != nil {
			return nil, "", err
		}
		hasher := sha256.New()
		prepared, err = o.prepareFrom(ctx, in.Collection, file, hasher)
		return prepared, digestOf(addLine(in.Collection, file), hex.EncodeToString(hasher.Sum(nil))), err
	}
	return nil, "", usage("a source or a url is required")
}

func (o *Operations) prepareFrom(ctx context.Context, collection string, file operation.File, content io.Writer) (*Prepared, error) {
	return o.service.PrepareStream(ctx, collection, &Stream{
		URI: file.Reference(), Name: file.Name(), MediaType: file.MediaType(), Size: file.Size(), Reader: io.TeeReader(file, content),
	})
}

type refreshNote struct {
	Label  string `json:"label"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	Chunks int    `json:"chunks,omitempty"`
}

const (
	noteUnchanged = "unchanged"
	noteSkipped   = "skipped"
	noteFailed    = "failed"
)

func (n *refreshNote) line() string {
	switch n.Status {
	case noteUnchanged:
		return fmt.Sprintf("unchanged: %s chunks=%d (a stored attachment never changes)", n.Label, n.Chunks)
	case noteSkipped:
		return fmt.Sprintf("skipped: %s (%s): refresh this source from the CLI", n.Label, n.Code)
	}
	return fmt.Sprintf("failed: %s (%s): the stored chunks are kept", n.Label, n.Code)
}

func (o *Operations) refresh(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	records, err := o.service.Sources(ctx, in.Collection, in.Source)
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	prepared := make([]*Prepared, 0, len(records))
	var notes []refreshNote
	for i := range records {
		next, note, err := o.prepareStored(ctx, call, in.Collection, &records[i])
		if err != nil {
			return operation.Result{}, moduleFailure(err)
		}
		if note != nil {
			notes = append(notes, *note)
			continue
		}
		prepared = append(prepared, next)
	}
	if len(prepared) == 0 && len(notes) > 0 && !slices.ContainsFunc(notes, func(n refreshNote) bool { return n.Status != noteFailed }) {
		return operation.Result{}, nothingRefreshed(notes)
	}
	var written []IngestResult
	committed, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		for _, item := range prepared {
			result, err := o.service.Write(ctx, ActionRefresh, item)
			if err != nil {
				return operation.Result{}, err
			}
			written = append(written, result)
		}
		return ingestResult(written, notes, embeddedAs{})
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	for i := range written {
		o.logger.InfoContext(ctx, "knowledge source refreshed", sourceAttrs("refresh", &written[i])...)
	}
	if !slicesChanged(written) {
		return committed, nil
	}
	return ingestResult(written, notes, o.embedAfterWrite(ctx, in.Collection))
}

func nothingRefreshed(notes []refreshNote) error {
	parts := make([]string, 0, len(notes))
	for i := range notes {
		parts = append(parts, notes[i].Label+" ("+notes[i].Code+")")
	}
	cause := Errorf(ErrorCodeNothingRefreshed, "no source could be refreshed and the stored chunks are kept: %s", strings.Join(parts, ", "))
	return operation.NotPerformed(ErrorCodeNothingRefreshed, operation.RenderedOnce[ErrorCode](cause))
}

func slicesChanged(results []IngestResult) bool {
	for i := range results {
		if results[i].Status != StatusUnchanged {
			return true
		}
	}
	return false
}

func (o *Operations) prepareStored(ctx context.Context, call *operation.Call, collection string, record *SourceRecord) (*Prepared, *refreshNote, error) {
	switch {
	case record.Kind == SourceKindURL:
		return o.settled(ctx, record, func() (*Prepared, error) { return o.service.PrepareURL(ctx, collection, record.URI) })
	case strings.HasPrefix(record.URI, artifactScheme):
		return nil, &refreshNote{Label: record.Label, Status: noteUnchanged, Chunks: record.Chunks}, nil
	case !canOpenStored(call.Caller.Surface, record.URI):
		return nil, &refreshNote{Label: record.Label, Status: noteSkipped, Code: string(operation.ErrorCodePolicyDenied)}, nil
	}
	return o.settled(ctx, record, func() (*Prepared, error) {
		file, err := call.Files.Open(ctx, record.URI)
		if err != nil {
			return nil, err
		}
		defer func() { _ = file.Close() }()
		return o.prepareFrom(ctx, collection, file, io.Discard)
	})
}

func canOpenStored(surface operation.Surface, uri string) bool {
	return surface == operation.SurfaceCLI || surface == operation.SurfaceAgent && !filepath.IsAbs(uri)
}

func (o *Operations) settled(ctx context.Context, record *SourceRecord, prepare func() (*Prepared, error)) (*Prepared, *refreshNote, error) {
	prepared, err := prepare()
	switch {
	case err == nil:
		return prepared, nil, nil
	case ctx.Err() != nil:
		return nil, nil, err
	}
	return nil, &refreshNote{Label: record.Label, Status: noteFailed, Code: reasonCode(err)}, nil
}

func reasonCode(err error) string {
	if code, coded := fault.CodeOf[ErrorCode](err); coded {
		return string(code)
	}
	if code, coded := fault.CodeOf[operation.ErrorCode](err); coded {
		return string(code)
	}
	return string(operation.ErrorCodeFailed)
}

type embeddedAs struct {
	state string
	sent  int
	code  string
}

func (o *Operations) embedAfterWrite(ctx context.Context, collection string) embeddedAs {
	outcome, err := o.service.EmbedAfterWrite(ctx, collection)
	if err != nil {
		code, _ := fault.CodeOf[ErrorCode](err)
		return embeddedAs{state: embeddingStateFailed, sent: outcome.Sent, code: string(code)}
	}
	return embeddedAs{state: outcome.State, sent: outcome.Sent}
}

func ingestResult(results []IngestResult, notes []refreshNote, embedded embeddedAs) (operation.Result, error) {
	type row struct {
		Status string `json:"status"`
		Label  string `json:"label"`
		Chunks int    `json:"chunks"`
		Added  int    `json:"chunks_added"`
		Remove int    `json:"chunks_removed"`
		Kept   int    `json:"chunks_kept"`
	}
	rows := make([]row, 0, len(results))
	lines := make([]string, 0, len(results)+len(notes)+1)
	for i := range results {
		result := &results[i]
		rows = append(rows, row{result.Status, result.Label, result.Chunks, result.ChunksAdded, result.ChunksRemoved, result.ChunksKept})
		lines = append(lines, fmt.Sprintf("%s: %s chunks=%d added=%d removed=%d kept=%d",
			result.Status, result.Label, result.Chunks, result.ChunksAdded, result.ChunksRemoved, result.ChunksKept))
	}
	for i := range notes {
		lines = append(lines, notes[i].line())
	}
	collection := ""
	if len(results) > 0 {
		collection = results[0].Collection
	}
	lines = append(lines, embeddingLines(collection, embedded)...)
	data := map[string]any{"sources": rows}
	if len(notes) > 0 {
		data["notes"] = notes
	}
	if embedded.state != embeddingNotApplied {
		data["embedding"] = embedded.state
		data["embedding_sent"] = embedded.sent
	}
	return linesResult(lines, data)
}

func embeddingLines(collection string, embedded embeddedAs) []string {
	switch embedded.state {
	case EmbedOutcomeEmbedded:
		return []string{fmt.Sprintf("embedding: %d chunks sent for vectors", embedded.sent)}
	case EmbedOutcomeReembedRequired:
		return []string{fmt.Sprintf("embedding: skipped (%s): the collection was embedded under another configuration; nothing was sent, run aura kb embed %s --apply", ErrorCodeReembedRequired, collection)}
	case EmbedOutcomeUnavailable:
		return []string{fmt.Sprintf("embedding: skipped (%s): embedding is not configured or its credential could not be resolved; nothing was sent", ErrorCodeEmbeddingUnavailable)}
	case embeddingStateFailed:
		return []string{fmt.Sprintf("embedding: failed (%s) after %d chunks; the source is stored, run aura kb embed %s --apply to resume", embedded.code, embedded.sent, collection)}
	}
	return nil
}

func digestOf(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func removalDigest(record *SourceRecord) string {
	return digestOf(record.Collection, record.URI, record.ID, record.ContentSHA256, strconv.Itoa(record.Chunks), strconv.Itoa(record.IngestVersion))
}

func (o *Operations) previewRemove(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Preview{}, err
	}
	record, err := o.service.PreviewRemove(ctx, in.Collection, in.Source)
	if err != nil {
		return operation.Preview{}, moduleFailure(err)
	}
	return operation.Preview{
		Lines: []string{
			fmt.Sprintf("would remove %d chunks of the source %s from collection %s", record.Chunks, record.URI, record.Collection),
			"this cannot be undone",
		},
		Digest: removalDigest(&record),
	}, nil
}

func (o *Operations) remove(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	var outcome RemoveOutcome
	var applied bool
	committed, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		if call.PreviewDigest != "" {
			record, err := o.service.PreviewRemove(ctx, in.Collection, in.Source)
			if err != nil {
				return operation.Result{}, err
			}
			if removalDigest(&record) != call.PreviewDigest {
				return operation.Result{}, staleSet("the source changed after the preview")
			}
		}
		removed, err := o.service.Remove(ctx, in.Collection, in.Source)
		if err != nil {
			return operation.Result{}, err
		}
		outcome, applied = removed, true
		return removedResult(&outcome, walUnchecked)
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if !applied {
		return committed, nil
	}
	o.logger.InfoContext(ctx, "knowledge source removed",
		"component", "knowledge", "operation", "remove", "collection", in.Collection, "chunks", outcome.Chunks)
	return removedResult(&outcome, o.truncateWAL(ctx))
}

type walState int

const (
	walUnchecked walState = iota
	walTruncated
	walBlocked
)

func (o *Operations) truncateWAL(ctx context.Context) walState {
	if truncated, err := o.service.TruncateWAL(ctx); err == nil && truncated {
		return walTruncated
	}
	return walBlocked
}

func staleSet(detail string) error {
	return operation.NotPerformed(operation.ErrorCodePreviewStale, operation.NewError(operation.ErrorCodePreviewStale, detail))
}

func removedResult(outcome *RemoveOutcome, wal walState) (operation.Result, error) {
	lines := make([]string, 0, len(removalNotes)+2)
	lines = append(lines, fmt.Sprintf("removed: %d chunks", outcome.Chunks))
	lines = append(lines, walLines(wal)...)
	lines = append(lines, removalNotes...)
	return linesResult(lines, walData(wal, map[string]any{"removed": outcome.Chunks}))
}

func walLines(wal walState) []string {
	if wal != walBlocked {
		return nil
	}
	return []string{"warning: the write-ahead log could not be truncated because a reader was active"}
}

func walData(wal walState, data map[string]any) map[string]any {
	if wal != walUnchecked {
		data["wal_truncated"] = wal == walTruncated
	}
	return data
}

func embedPreviewLines(preview *EmbedPreview) []string {
	lines := []string{
		fmt.Sprintf("collection %s would send %d of %d chunks (about %d tokens, estimated) using model %s at %d dimensions",
			preview.Collection, preview.Pending, preview.Chunks, preview.EstimatedTokens, preview.Model, preview.Dimensions),
		destinationLine(preview.Destination, preview.Loopback),
	}
	if preview.Reembed {
		lines = append(lines, "reembed: the stored model or destination differs from the configuration; vectors are rebuilt and none are searched until the run completes")
	}
	lines = append(lines, queryPolicyLine(preview.PublicQueries))
	if preview.Pending == 0 {
		lines = append(lines, "nothing to send: every chunk already has a vector")
	}
	return lines
}

func destinationLine(destination string, loopback bool) string {
	if loopback {
		return fmt.Sprintf("destination: %s (this host; nothing leaves it)", destination)
	}
	return fmt.Sprintf("destination: %s (text leaves this host)", destination)
}

func queryPolicyLine(publicQueries bool) string {
	if publicQueries {
		return "queries: owner, trusted and public queries are sent to the same destination once the collection is fully embedded (knowledge.embedding.public_queries is true)"
	}
	return "queries: owner and trusted queries are sent to the same destination once the collection is fully embedded; public queries are not sent (knowledge.embedding.public_queries is false)"
}

func (o *Operations) previewEmbed(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Preview{}, err
	}
	preview, err := o.service.EmbedPreview(ctx, in.Collection)
	if err != nil {
		return operation.Preview{}, moduleFailure(err)
	}
	lines := embedPreviewLines(&preview)
	return operation.Preview{Lines: lines, Digest: digestOf(lines...)}, nil
}

func (o *Operations) embed(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	if call.PreviewDigest != "" {
		preview, err := o.service.EmbedPreview(ctx, in.Collection)
		if err != nil {
			return operation.Result{}, refusedBeforeSend(err)
		}
		if digestOf(embedPreviewLines(&preview)...) != call.PreviewDigest {
			return operation.Result{}, staleSet("the embedding destination or the chunks changed after the preview")
		}
	}
	result, err := o.service.Embed(ctx, in.Collection)
	if err != nil {
		if result.Batches == 0 {
			return operation.Result{}, refusedBeforeSend(err)
		}
		return operation.Result{}, moduleFailure(withHint(err, fmt.Sprintf("%d chunks were sent, rerun aura kb embed %s --apply to resume", result.Sent, in.Collection)))
	}
	o.logger.InfoContext(ctx, "knowledge collection embedded",
		"component", "knowledge", "operation", "embed", "collection", in.Collection,
		"sent", result.Sent, "batches", result.Batches, "completed", result.Completed)
	lines := []string{fmt.Sprintf("embedded: %d chunks in %d batches sent to %s", result.Sent, result.Batches, result.Destination)}
	if result.Completed {
		lines = append(lines, "complete: every chunk has a vector")
	} else {
		lines = append(lines, fmt.Sprintf("partial: rerun aura kb embed %s --apply to resume", in.Collection))
	}
	return linesResult(lines, map[string]any{
		"sent": result.Sent, "batches": result.Batches, "destination": result.Destination, "completed": result.Completed,
	})
}

func withHint(err error, hint string) error {
	var coded *fault.Error[ErrorCode]
	if !errors.As(err, &coded) {
		return err
	}
	return fault.Wrap(coded.Code, coded.Detail+"; "+hint, err)
}

var refusalsBeforeSend = []ErrorCode{
	ErrorCodeCollectionInvalid, ErrorCodeSourceNotFound, ErrorCodeEmbeddingUnavailable,
	ErrorCodeSecretInvalid, ErrorCodeSecretUnavailable, ErrorCodeReembedRequired,
}

func refusedBeforeSend(err error) error {
	code, coded := fault.CodeOf[ErrorCode](err)
	if coded && slices.Contains(refusalsBeforeSend, code) {
		return operation.NotPerformed(code, operation.RenderedOnce[ErrorCode](err))
	}
	return moduleFailure(err)
}

func unembedDigest(collection string, outcome *DisableOutcome) string {
	return digestOf(collection, strconv.FormatBool(outcome.OptedIn), strconv.Itoa(outcome.Vectors))
}

func (o *Operations) previewUnembed(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Preview{}, err
	}
	outcome, err := o.service.PreviewEmbedOff(ctx, in.Collection)
	if err != nil {
		return operation.Preview{}, moduleFailure(err)
	}
	return operation.Preview{
		Lines:  []string{fmt.Sprintf("would delete %d vectors of collection %s and clear its embedding opt-in", outcome.Vectors, in.Collection)},
		Digest: unembedDigest(in.Collection, &outcome),
		Empty:  !outcome.OptedIn && outcome.Vectors == 0,
	}, nil
}

func (o *Operations) unembed(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	var outcome DisableOutcome
	var applied bool
	committed, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		if call.PreviewDigest != "" {
			current, err := o.service.PreviewEmbedOff(ctx, in.Collection)
			if err != nil {
				return operation.Result{}, err
			}
			if unembedDigest(in.Collection, &current) != call.PreviewDigest {
				return operation.Result{}, staleSet("the vectors changed after the preview")
			}
		}
		disabled, err := o.service.EmbedOff(ctx, in.Collection)
		if err != nil {
			return operation.Result{}, err
		}
		outcome, applied = disabled, true
		return unembeddedResult(&outcome, walUnchecked)
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if !applied {
		return committed, nil
	}
	o.logger.InfoContext(ctx, "knowledge embedding switched off",
		"component", "knowledge", "operation", "embed_off", "collection", in.Collection, "vectors", outcome.Vectors)
	return unembeddedResult(&outcome, o.truncateWAL(ctx))
}

func unembeddedResult(outcome *DisableOutcome, wal walState) (operation.Result, error) {
	lines := make([]string, 0, len(removalNotes)+2)
	lines = append(lines, fmt.Sprintf("embedding off: %d vectors deleted and the opt-in cleared", outcome.Vectors))
	lines = append(lines, walLines(wal)...)
	lines = append(lines, removalNotes...)
	return linesResult(lines, walData(wal, map[string]any{"vectors": outcome.Vectors}))
}

func trustDigest(detail *CollectionDetail) string {
	parts := make([]string, 0, 2+len(detail.Sources))
	parts = append(parts, detail.ID, strconv.FormatInt(detail.Generation, 10))
	sources := slices.Clone(detail.Sources)
	slices.SortFunc(sources, func(a, b SourceRecord) int {
		return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.URI, b.URI))
	})
	for i := range sources {
		parts = append(parts, sources[i].ID+"|"+sources[i].ContentSHA256)
	}
	return digestOf(parts...)
}

func trustPreviewLines(detail *CollectionDetail) []string {
	shown := min(len(detail.Sources), maxTrustPreviewSources)
	lines := make([]string, 0, shown+3)
	lines = append(lines, fmt.Sprintf("collection %s: %d source(s), %d chunk(s), currently %s",
		detail.ID, len(detail.Sources), detail.Chunks, trustState(detail.Trust)))
	for i := range shown {
		source := &detail.Sources[i]
		status := "not vouched"
		if detail.Trust == TrustTrusted && source.Vouched() {
			status = "vouched"
		}
		lines = append(lines, fmt.Sprintf("- %s (%s) %s [%s]", source.Label, source.Kind, source.URI, status))
	}
	if extra := len(detail.Sources) - shown; extra > 0 {
		lines = append(lines, fmt.Sprintf("and %d more", extra))
	}
	lines = append(lines, trustWarningLine)
	return lines
}

func (o *Operations) previewTrust(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Preview{}, err
	}
	detail, err := o.service.Show(ctx, in.Collection)
	if err != nil {
		return operation.Preview{}, moduleFailure(err)
	}
	return operation.Preview{
		Lines:  trustPreviewLines(&detail),
		Digest: trustDigest(&detail),
	}, nil
}

func (o *Operations) trust(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	var outcome TrustOutcome
	var applied bool
	committed, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		detail, err := o.service.Show(ctx, in.Collection)
		if err != nil {
			return operation.Result{}, err
		}
		if call.PreviewDigest == "" || trustDigest(&detail) != call.PreviewDigest {
			return operation.Result{}, staleSet("the collection changed after the preview")
		}
		trusted, err := o.service.Trust(ctx, in.Collection)
		if err != nil {
			return operation.Result{}, err
		}
		outcome, applied = trusted, true
		return linesResult(
			[]string{fmt.Sprintf("trusted: %s (%d source(s) vouched)", trusted.Collection, trusted.Vouched)},
			map[string]any{
				"collection": trusted.Collection,
				"trust":      TrustTrusted,
				"trusted_at": trusted.TrustedAt.UTC().Format(time.RFC3339),
				"vouched":    trusted.Vouched,
			},
		)
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if !applied {
		return committed, nil
	}
	o.logger.InfoContext(ctx, "knowledge collection trusted",
		"component", "knowledge", "operation", "trust", "collection", outcome.Collection, "sources", outcome.Vouched)
	return committed, nil
}

func (o *Operations) untrust(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	var outcome UntrustOutcome
	var applied bool
	committed, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		untrusted, err := o.service.Untrust(ctx, in.Collection)
		if err != nil {
			return operation.Result{}, err
		}
		outcome, applied = untrusted, true
		return linesResult(
			[]string{fmt.Sprintf("untrusted: %s (cleared vouching on %d source(s))", untrusted.Collection, untrusted.Cleared)},
			map[string]any{
				"collection": untrusted.Collection,
				"trust":      TrustUntrusted,
				"cleared":    untrusted.Cleared,
			},
		)
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if !applied {
		return committed, nil
	}
	o.logger.InfoContext(ctx, "knowledge collection untrusted",
		"component", "knowledge", "operation", "untrust", "collection", outcome.Collection, "sources", outcome.Cleared)
	return committed, nil
}

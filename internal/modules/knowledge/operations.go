package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
)

const (
	digestPrefixLength = 12
	workspaceScheme    = "workspace:"
	artifactScheme     = "artifact:"

	embeddingStateFailed = "failed"
	embeddingNotApplied  = ""
)

var removalNotes = []string{
	"not removed: earlier replies, recorded tool results, the durable runtime journal and older backups may still hold copies",
	"not removed: copies an embedding provider holds under its own terms, and vectors a running server keeps in memory until its next retrieval or restart",
	"raw database pages can keep fragments until the database is vacuumed",
}

type OperationDeps struct {
	Service *Service
}

type Operations struct {
	service *Service
}

func NewOperations(deps *OperationDeps) (*Operations, error) {
	if deps == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "knowledge operation deps must not be nil")
	}
	return &Operations{service: deps.Service}, nil
}

func (o *Operations) Bindings() []operation.Binding {
	return []operation.Binding{
		{ID: operationList, Handle: o.list},
		{ID: operationShow, Handle: o.show},
		{ID: operationCheck, Handle: o.check},
		{ID: operationAdd, Handle: o.add},
		{ID: operationRefresh, Handle: o.refresh},
		{ID: operationRemove, Handle: o.remove, Preview: o.previewRemove},
		{ID: operationEmbed, Handle: o.embed, Preview: o.previewEmbed},
		{ID: operationUnembed, Handle: o.unembed, Preview: o.previewUnembed},
	}
}

type collectionInput struct {
	Collection string `json:"collection"`
	Source     string `json:"source"`
	URL        string `json:"url"`
}

func usage(detail string) error {
	return operation.NewError(operation.ErrorCodeInputInvalid, detail)
}

func moduleFailure(err error) error {
	if err == nil {
		return nil
	}
	if _, coded := fault.CodeOf[operation.ErrorCode](err); coded {
		return err
	}
	if code, coded := fault.CodeOf[ErrorCode](err); coded {
		return operation.Fail(code, err)
	}
	return err
}

func (o *Operations) ready(call *operation.Call, input *collectionInput) error {
	if o.service == nil {
		return Errorf(ErrorCodeInvalidArgument, "knowledge service must not be nil")
	}
	return call.Decode(input)
}

func linesResult(lines []string, data map[string]any) (operation.Result, error) {
	data["text"] = strings.Join(lines, "\n")
	return operation.JSONResult(data)
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
		Collection string `json:"collection"`
		Sources    int    `json:"sources"`
		Chunks     int    `json:"chunks"`
		Generation int64  `json:"generation"`
	}
	rows := make([]row, 0, len(collections))
	for i := range collections {
		rows = append(rows, row{
			Collection: collections[i].ID, Sources: collections[i].Sources,
			Chunks: collections[i].Chunks, Generation: collections[i].Generation,
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
	}
	lines := []string{fmt.Sprintf("collection: %s sources=%d chunks=%d", detail.ID, len(detail.Sources), detail.Chunks)}
	sources := make([]listed, 0, len(detail.Sources))
	for i := range detail.Sources {
		source := &detail.Sources[i]
		entry := listed{
			URI: source.URI, Label: source.Label, Kind: source.Kind, Chunks: source.Chunks,
			Digest:   source.ContentSHA256[:min(digestPrefixLength, len(source.ContentSHA256))],
			Ingested: source.IngestedAt.UTC().Format(time.RFC3339),
		}
		sources = append(sources, entry)
		lines = append(lines, fmt.Sprintf("source: %s label=%s kind=%s chunks=%d digest=%s ingested=%s",
			entry.URI, entry.Label, entry.Kind, entry.Chunks, entry.Digest, entry.Ingested))
	}
	return linesResult(lines, map[string]any{"collection": detail.ID, "chunks": detail.Chunks, "sources": sources})
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
	lines := []string{
		fmt.Sprintf("collections=%d sources=%d chunks=%d indexed=%d", report.Collections, report.Sources, report.Chunks, report.IndexedChunks),
		"ok",
	}
	return linesResult(lines, map[string]any{
		"collections": report.Collections, "sources": report.Sources, "chunks": report.Chunks, "indexed_chunks": report.IndexedChunks,
	})
}

func (o *Operations) add(ctx context.Context, call *operation.Call) (operation.Result, error) {
	var in collectionInput
	if err := o.ready(call, &in); err != nil {
		return operation.Result{}, err
	}
	prepared, err := o.prepareAdd(ctx, call, &in)
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	var written IngestResult
	var applied bool
	committed, err := call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		result, err := o.service.Write(ctx, ActionAdd, prepared)
		if err != nil {
			return operation.Result{}, err
		}
		written, applied = result, true
		return ingestResult([]IngestResult{result}, embeddedAs{})
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if !applied || written.Status == StatusUnchanged {
		return committed, nil
	}
	return ingestResult([]IngestResult{written}, o.embedAfterWrite(ctx, in.Collection))
}

func (o *Operations) prepareAdd(ctx context.Context, call *operation.Call, in *collectionInput) (*Prepared, error) {
	switch {
	case (in.Source == "") == (in.URL == ""):
		return nil, usage("exactly one of source or url is required")
	case in.URL != "":
		return o.service.PrepareURL(ctx, in.Collection, in.URL)
	case IsURLReference(in.Source):
		return o.service.PrepareURL(ctx, in.Collection, in.Source)
	case call.Caller.Surface == operation.SurfaceCLI:
		return o.service.PrepareHostFile(ctx, in.Collection, in.Source)
	}
	return o.prepareOpened(ctx, call, in.Collection, in.Source)
}

func (o *Operations) prepareOpened(ctx context.Context, call *operation.Call, collection, source string) (*Prepared, error) {
	if call.Files == nil {
		return nil, operation.NewError(operation.ErrorCodePolicyDenied, "file sources are not available")
	}
	file, err := call.Files.Open(ctx, source)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return o.service.PrepareStream(ctx, collection, &Stream{
		URI: storedURI(source), Name: file.Name(), MediaType: file.MediaType(), Size: file.Size(), Reader: file,
	})
}

func storedURI(source string) string {
	if strings.HasPrefix(source, artifactScheme) {
		return source
	}
	return workspaceScheme + path.Clean(filepath.ToSlash(source))
}

func openableSource(uri string) string {
	return strings.TrimPrefix(uri, workspaceScheme)
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
	var failures []error
	for i := range records {
		next, err := o.prepareStored(ctx, call, in.Collection, &records[i])
		if err != nil {
			failures = append(failures, fmt.Errorf("refresh %s: %w", records[i].Label, err))
			continue
		}
		prepared = append(prepared, next)
	}
	if len(failures) > 0 {
		return operation.Result{}, moduleFailure(errors.Join(failures...))
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
		return ingestResult(written, embeddedAs{})
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if !slicesChanged(written) {
		return committed, nil
	}
	return ingestResult(written, o.embedAfterWrite(ctx, in.Collection))
}

func slicesChanged(results []IngestResult) bool {
	for i := range results {
		if results[i].Status != StatusUnchanged {
			return true
		}
	}
	return false
}

func (o *Operations) prepareStored(ctx context.Context, call *operation.Call, collection string, record *SourceRecord) (*Prepared, error) {
	switch {
	case record.Kind == SourceKindURL:
		return o.service.PrepareURL(ctx, collection, record.URI)
	case call.Caller.Surface != operation.SurfaceAgent && filepath.IsAbs(record.URI):
		return o.service.PrepareHostFile(ctx, collection, record.URI)
	}
	return o.prepareOpened(ctx, call, collection, openableSource(record.URI))
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

func ingestResult(results []IngestResult, embedded embeddedAs) (operation.Result, error) {
	type row struct {
		Status string `json:"status"`
		Label  string `json:"label"`
		Chunks int    `json:"chunks"`
		Added  int    `json:"chunks_added"`
		Remove int    `json:"chunks_removed"`
		Kept   int    `json:"chunks_kept"`
	}
	rows := make([]row, 0, len(results))
	lines := make([]string, 0, len(results)+1)
	for i := range results {
		result := &results[i]
		rows = append(rows, row{result.Status, result.Label, result.Chunks, result.ChunksAdded, result.ChunksRemoved, result.ChunksKept})
		lines = append(lines, fmt.Sprintf("%s: %s chunks=%d added=%d removed=%d kept=%d",
			result.Status, result.Label, result.Chunks, result.ChunksAdded, result.ChunksRemoved, result.ChunksKept))
	}
	collection := ""
	if len(results) > 0 {
		collection = results[0].Collection
	}
	lines = append(lines, embeddingLines(collection, embedded)...)
	data := map[string]any{"sources": rows}
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
		return []string{fmt.Sprintf("embedding: skipped (%s): the collection was embedded under another configuration; nothing was sent, run aura kb embed %s", ErrorCodeReembedRequired, collection)}
	case EmbedOutcomeUnavailable:
		return []string{fmt.Sprintf("embedding: skipped (%s): embedding is not configured or its credential could not be resolved; nothing was sent", ErrorCodeEmbeddingUnavailable)}
	case embeddingStateFailed:
		return []string{fmt.Sprintf("embedding: failed (%s) after %d chunks; the source is stored, run aura kb embed %s to resume", embedded.code, embedded.sent, collection)}
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
		return removedResult(&outcome, true)
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if !applied {
		return committed, nil
	}
	truncated, err := o.service.TruncateWAL(ctx)
	if err != nil {
		truncated = false
	}
	return removedResult(&outcome, truncated)
}

func staleSet(detail string) error {
	return operation.NotPerformed(operation.ErrorCodePreviewStale, operation.NewError(operation.ErrorCodePreviewStale, detail))
}

func removedResult(outcome *RemoveOutcome, truncated bool) (operation.Result, error) {
	lines := make([]string, 0, len(removalNotes)+2)
	lines = append(lines, fmt.Sprintf("removed: %d chunks", outcome.Chunks))
	lines = append(lines, walLines(truncated)...)
	lines = append(lines, removalNotes...)
	return linesResult(lines, map[string]any{"removed": outcome.Chunks, "wal_truncated": truncated})
}

func walLines(truncated bool) []string {
	if truncated {
		return nil
	}
	return []string{"warning: the write-ahead log could not be truncated because a reader was active"}
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
		return operation.Result{}, moduleFailure(err)
	}
	lines := []string{fmt.Sprintf("embedded: %d chunks in %d batches sent to %s", result.Sent, result.Batches, result.Destination)}
	if result.Completed {
		lines = append(lines, "complete: every chunk has a vector")
	} else {
		lines = append(lines, fmt.Sprintf("partial: rerun aura kb embed %s to resume", in.Collection))
	}
	return linesResult(lines, map[string]any{
		"sent": result.Sent, "batches": result.Batches, "destination": result.Destination, "completed": result.Completed,
	})
}

var refusalsBeforeSend = []ErrorCode{
	ErrorCodeCollectionInvalid, ErrorCodeSourceNotFound, ErrorCodeEmbeddingUnavailable,
	ErrorCodeSecretInvalid, ErrorCodeSecretUnavailable, ErrorCodeReembedRequired,
}

func refusedBeforeSend(err error) error {
	code, coded := fault.CodeOf[ErrorCode](err)
	if coded && slices.Contains(refusalsBeforeSend, code) {
		return operation.NotPerformed(code, err)
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
		return unembeddedResult(&outcome, true)
	})
	if err != nil {
		return operation.Result{}, moduleFailure(err)
	}
	if !applied {
		return committed, nil
	}
	truncated, err := o.service.TruncateWAL(ctx)
	if err != nil {
		truncated = false
	}
	return unembeddedResult(&outcome, truncated)
}

func unembeddedResult(outcome *DisableOutcome, truncated bool) (operation.Result, error) {
	lines := make([]string, 0, len(removalNotes)+2)
	lines = append(lines, fmt.Sprintf("embedding off: %d vectors deleted and the opt-in cleared", outcome.Vectors))
	lines = append(lines, walLines(truncated)...)
	lines = append(lines, removalNotes...)
	return linesResult(lines, map[string]any{"vectors": outcome.Vectors, "wal_truncated": truncated})
}

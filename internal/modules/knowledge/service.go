package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	SourceKindFile = "file"
	SourceKindURL  = "url"

	StatusAdded     = "added"
	StatusReplaced  = "replaced"
	StatusUnchanged = "unchanged"

	ActionAdd     = "add"
	ActionRefresh = "refresh"
	ActionRemove  = "remove"

	ActionEmbed    = "embed"
	ActionEmbedOff = "embed_off"
)

type HTMLReducer func(ctx context.Context, source []byte, maxBytes int64) (markdown string, truncated bool, err error)

type WriteResult struct {
	Status        string
	SourceID      string
	Chunks        int
	ChunksAdded   int
	ChunksRemoved int
	ChunksKept    int
}

type SourceWrite struct {
	Collection    string
	Kind          string
	URI           string
	Label         string
	ContentSHA256 string
	IngestVersion int
	IngestedAt    time.Time
	Chunks        []Chunk
	CollectionCap int
	TotalCap      int
}

type CollectionSummary struct {
	ID         string
	Generation int64
	Sources    int
	Chunks     int
	CreatedAt  time.Time
}

type SourceRecord struct {
	ID            string
	Collection    string
	Kind          string
	URI           string
	Label         string
	ContentSHA256 string
	IngestVersion int
	IngestedAt    time.Time
	Chunks        int
}

type RemoveOutcome struct {
	SourceID string
	Chunks   int
}

type IndexReport struct {
	Collections   int
	Sources       int
	Chunks        int
	IndexedChunks int
	Problems      []string
}

type Store interface {
	ReplaceSource(ctx context.Context, write *SourceWrite) (WriteResult, error)
	ListCollections(ctx context.Context) ([]CollectionSummary, error)
	ListSources(ctx context.Context, collection string) ([]SourceRecord, error)
	GetSource(ctx context.Context, collection, uri string) (SourceRecord, bool, error)
	DeleteSource(ctx context.Context, collection, uri string) (RemoveOutcome, error)
	TruncateWAL(ctx context.Context) (bool, error)
	Check(ctx context.Context) (IndexReport, error)
}

type OwnerAction struct {
	Action     string
	Collection string
	SourceID   string
	Kind       string
	Outcome    string
	Chunks     int
	At         time.Time
}

type ActionSink interface {
	RecordOwnerAction(ctx context.Context, action *OwnerAction) error
}

type Deps struct {
	Store     Store
	Secrets   SecretSource
	Reducer   HTMLReducer
	HTTP      HTTPDoer
	Actions   ActionSink
	Vectors   EmbeddingStore
	Embedder  Embedder
	Embedding *EmbeddingSettings
	Now       func() time.Time
}

type Service struct {
	store     Store
	secrets   SecretSource
	reducer   HTMLReducer
	http      HTTPDoer
	actions   ActionSink
	vectors   EmbeddingStore
	embedder  Embedder
	embedding *EmbeddingSettings
	now       func() time.Time
}

func NewService(deps *Deps) (*Service, error) {
	if deps == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "dependencies must not be nil")
	}
	if deps.Store == nil || deps.Secrets == nil || deps.Reducer == nil || deps.HTTP == nil || deps.Actions == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "store, secrets, reducer, http and actions are required")
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	if deps.Embedding != nil && !deps.Embedding.valid() {
		return nil, Errorf(ErrorCodeInvalidArgument, "embedding settings need a fingerprint, destination, dimensions and batch size")
	}
	return &Service{
		store: deps.Store, secrets: deps.Secrets, reducer: deps.Reducer, http: deps.HTTP, actions: deps.Actions,
		vectors: deps.Vectors, embedder: deps.Embedder, embedding: deps.Embedding, now: now,
	}, nil
}

func ValidCollectionID(id string) bool {
	return auraagent.ValidKnowledgeCollectionID(id)
}

func requireCollection(id string) error {
	if !ValidCollectionID(id) {
		return Errorf(ErrorCodeCollectionInvalid, "collection id must match %s", auraagent.KnowledgeCollectionIDPattern)
	}
	return nil
}

type IngestResult struct {
	Collection string
	Kind       string
	URI        string
	Label      string
	Embedding  EmbedOutcome
	WriteResult
}

type Prepared struct {
	collection string
	kind       string
	uri        string
	label      string
	sha        string
	chunks     []Chunk
}

type Stream struct {
	URI       string
	Name      string
	MediaType string
	Size      int64
	Reader    io.Reader
}

func (s *Service) PrepareURL(ctx context.Context, collection, raw string) (*Prepared, error) {
	return s.prepare(ctx, collection, func() (*loadedSource, error) { return loadURL(ctx, s.http, raw) })
}

func (s *Service) PrepareStream(ctx context.Context, collection string, stream *Stream) (*Prepared, error) {
	if stream == nil || stream.Reader == nil {
		return nil, Errorf(ErrorCodeInvalidArgument, "source stream must not be nil")
	}
	return s.prepare(ctx, collection, func() (*loadedSource, error) { return loadStream(stream) })
}

func IsURLReference(source string) bool {
	return strings.Contains(source, "://")
}

func (s *Service) Sources(ctx context.Context, collection, source string) ([]SourceRecord, error) {
	if err := requireCollection(collection); err != nil {
		return nil, err
	}
	records, err := s.store.ListSources(ctx, collection)
	if err != nil {
		return nil, err
	}
	if source != "" {
		records = filterSources(records, source)
	}
	if len(records) == 0 {
		return nil, Errorf(ErrorCodeSourceNotFound, "no matching source in collection")
	}
	return records, nil
}

func filterSources(records []SourceRecord, uri string) []SourceRecord {
	var matched []SourceRecord
	for i := range records {
		if records[i].URI == uri {
			matched = append(matched, records[i])
		}
	}
	return matched
}

func (s *Service) prepare(ctx context.Context, collection string, load func() (*loadedSource, error)) (*Prepared, error) {
	if err := requireCollection(collection); err != nil {
		return nil, err
	}
	screen, err := s.secrets.Snapshot(ctx)
	if err != nil {
		return nil, fault.Wrap(ErrorCodeSecretUnavailable, "a configured secret reference could not be resolved", err)
	}
	loaded, err := load()
	if err != nil {
		return nil, err
	}
	text, err := s.readableText(ctx, loaded)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, Errorf(ErrorCodeSourceUnsupported, "source has no readable text")
	}
	if err := screenText(loaded.uri+"\n"+text, screen); err != nil {
		return nil, err
	}
	chunks, exceeded := SplitChunks(text, MaxChunksPerSource)
	if exceeded {
		return nil, Errorf(ErrorCodeCapExceeded, "source exceeds %d chunks", MaxChunksPerSource)
	}
	if len(chunks) == 0 {
		return nil, Errorf(ErrorCodeSourceUnsupported, "source has no readable text")
	}
	sum := sha256.Sum256([]byte(text))
	return &Prepared{
		collection: collection, kind: loaded.kind, uri: loaded.uri, label: loaded.label,
		sha: hex.EncodeToString(sum[:]), chunks: chunks,
	}, nil
}

func (s *Service) Write(ctx context.Context, action string, prepared *Prepared) (IngestResult, error) {
	if prepared == nil {
		return IngestResult{}, Errorf(ErrorCodeInvalidArgument, "prepared source must not be nil")
	}
	written, err := s.store.ReplaceSource(ctx, &SourceWrite{
		Collection:    prepared.collection,
		Kind:          prepared.kind,
		URI:           prepared.uri,
		Label:         prepared.label,
		ContentSHA256: prepared.sha,
		IngestVersion: IngestVersion,
		IngestedAt:    s.now().UTC(),
		Chunks:        prepared.chunks,
		CollectionCap: MaxChunksPerCollection,
		TotalCap:      MaxChunksTotal,
	})
	if err != nil {
		return IngestResult{}, err
	}
	result := IngestResult{Collection: prepared.collection, Kind: prepared.kind, URI: prepared.uri, Label: prepared.label, WriteResult: written}
	if err := s.actions.RecordOwnerAction(ctx, &OwnerAction{
		Action: action, Collection: prepared.collection, SourceID: written.SourceID, Kind: prepared.kind,
		Outcome: written.Status, Chunks: written.Chunks, At: s.now().UTC(),
	}); err != nil {
		return IngestResult{}, fmt.Errorf("record owner action: %w", err)
	}
	return result, nil
}

func (s *Service) readableText(ctx context.Context, loaded *loadedSource) (string, error) {
	if loaded.format != FormatHTML {
		return Normalize(string(loaded.data), loaded.format), nil
	}
	reduced, truncated, err := s.reducer(ctx, loaded.data, MaxSourceBytes)
	if err != nil {
		return "", err
	}
	if truncated {
		return "", Errorf(ErrorCodeSourceTooLarge, "reduced source exceeds %d bytes", MaxSourceBytes)
	}
	return Normalize(reduced, FormatText), nil
}

func (s *Service) List(ctx context.Context) ([]CollectionSummary, error) {
	return s.store.ListCollections(ctx)
}

type CollectionDetail struct {
	ID      string
	Sources []SourceRecord
	Chunks  int
}

func (s *Service) Show(ctx context.Context, collection string) (CollectionDetail, error) {
	if err := requireCollection(collection); err != nil {
		return CollectionDetail{}, err
	}
	records, err := s.store.ListSources(ctx, collection)
	if err != nil {
		return CollectionDetail{}, err
	}
	if len(records) == 0 {
		return CollectionDetail{}, Errorf(ErrorCodeSourceNotFound, "collection has no sources")
	}
	detail := CollectionDetail{ID: collection, Sources: records}
	for i := range records {
		detail.Chunks += records[i].Chunks
	}
	return detail, nil
}

func (s *Service) PreviewRemove(ctx context.Context, collection, source string) (SourceRecord, error) {
	if err := requireCollection(collection); err != nil {
		return SourceRecord{}, err
	}
	if source == "" {
		return SourceRecord{}, Errorf(ErrorCodeInvalidArgument, "source must not be empty")
	}
	record, found, err := s.store.GetSource(ctx, collection, source)
	if err != nil {
		return SourceRecord{}, err
	}
	if !found {
		return SourceRecord{}, Errorf(ErrorCodeSourceNotFound, "knowledge source does not exist")
	}
	return record, nil
}

func (s *Service) Remove(ctx context.Context, collection, source string) (RemoveOutcome, error) {
	if err := requireCollection(collection); err != nil {
		return RemoveOutcome{}, err
	}
	outcome, err := s.store.DeleteSource(ctx, collection, source)
	if err != nil {
		return RemoveOutcome{}, err
	}
	if err := s.actions.RecordOwnerAction(ctx, &OwnerAction{
		Action: ActionRemove, Collection: collection, SourceID: outcome.SourceID,
		Outcome: "removed", Chunks: outcome.Chunks, At: s.now().UTC(),
	}); err != nil {
		return RemoveOutcome{}, fmt.Errorf("record owner action: %w", err)
	}
	return outcome, nil
}

func (s *Service) TruncateWAL(ctx context.Context) (bool, error) {
	return s.store.TruncateWAL(ctx)
}

func (s *Service) Check(ctx context.Context) (IndexReport, error) {
	report, err := s.store.Check(ctx)
	if err != nil {
		return IndexReport{}, err
	}
	if len(report.Problems) > 0 {
		return report, Errorf(ErrorCodeIndexDamaged, "knowledge index check found %d problem(s)", len(report.Problems))
	}
	return report, nil
}

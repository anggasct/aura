package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
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
)

var collectionIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,47}$`)

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
	SourceID     string
	Chunks       int
	Applied      bool
	WALTruncated bool
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
	RemoveSource(ctx context.Context, collection, uri string, apply bool) (RemoveOutcome, error)
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
	Store   Store
	Secrets SecretSource
	Reducer HTMLReducer
	HTTP    HTTPDoer
	Actions ActionSink
	Now     func() time.Time
}

type Service struct {
	store   Store
	secrets SecretSource
	reducer HTMLReducer
	http    HTTPDoer
	actions ActionSink
	now     func() time.Time
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
	return &Service{store: deps.Store, secrets: deps.Secrets, reducer: deps.Reducer, http: deps.HTTP, actions: deps.Actions, now: now}, nil
}

func ValidCollectionID(id string) bool {
	return collectionIDPattern.MatchString(id)
}

func requireCollection(id string) error {
	if !ValidCollectionID(id) {
		return Errorf(ErrorCodeCollectionInvalid, "collection id must match %s", collectionIDPattern.String())
	}
	return nil
}

type IngestResult struct {
	Collection string
	Kind       string
	URI        string
	Label      string
	WriteResult
}

func (s *Service) Add(ctx context.Context, collection, source string) (IngestResult, error) {
	if err := requireCollection(collection); err != nil {
		return IngestResult{}, err
	}
	if strings.TrimSpace(source) == "" {
		return IngestResult{}, Errorf(ErrorCodeInvalidArgument, "source must not be empty")
	}
	return s.ingest(ctx, ActionAdd, collection, source, isURLReference(source))
}

func isURLReference(source string) bool {
	return strings.Contains(source, "://")
}

func (s *Service) Refresh(ctx context.Context, collection, source string) ([]IngestResult, error) {
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
	var results []IngestResult
	var failures []error
	for i := range records {
		result, err := s.ingest(ctx, ActionRefresh, collection, records[i].URI, records[i].Kind == SourceKindURL)
		if err != nil {
			failures = append(failures, fmt.Errorf("refresh %s: %w", records[i].Label, err))
			continue
		}
		results = append(results, result)
	}
	return results, errors.Join(failures...)
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

func (s *Service) ingest(ctx context.Context, action, collection, source string, isURL bool) (IngestResult, error) {
	screen, err := s.secrets.Snapshot(ctx)
	if err != nil {
		return IngestResult{}, fmt.Errorf("%w: %w", &Error{Code: ErrorCodeSecretUnavailable, Detail: "a configured secret reference could not be resolved"}, err)
	}
	loaded, err := s.load(ctx, source, isURL)
	if err != nil {
		return IngestResult{}, err
	}
	text, err := s.readableText(ctx, loaded)
	if err != nil {
		return IngestResult{}, err
	}
	if strings.TrimSpace(text) == "" {
		return IngestResult{}, Errorf(ErrorCodeSourceUnsupported, "source has no readable text")
	}
	if err := screenText(loaded.uri+"\n"+text, screen); err != nil {
		return IngestResult{}, err
	}
	chunks, exceeded := SplitChunks(text, MaxChunksPerSource)
	if exceeded {
		return IngestResult{}, Errorf(ErrorCodeCapExceeded, "source exceeds %d chunks", MaxChunksPerSource)
	}
	if len(chunks) == 0 {
		return IngestResult{}, Errorf(ErrorCodeSourceUnsupported, "source has no readable text")
	}
	sum := sha256.Sum256([]byte(text))
	written, err := s.store.ReplaceSource(ctx, &SourceWrite{
		Collection:    collection,
		Kind:          loaded.kind,
		URI:           loaded.uri,
		Label:         loaded.label,
		ContentSHA256: hex.EncodeToString(sum[:]),
		IngestVersion: IngestVersion,
		IngestedAt:    s.now().UTC(),
		Chunks:        chunks,
		CollectionCap: MaxChunksPerCollection,
		TotalCap:      MaxChunksTotal,
	})
	if err != nil {
		return IngestResult{}, err
	}
	result := IngestResult{Collection: collection, Kind: loaded.kind, URI: loaded.uri, Label: loaded.label, WriteResult: written}
	if err := s.actions.RecordOwnerAction(ctx, &OwnerAction{
		Action: action, Collection: collection, SourceID: written.SourceID, Kind: loaded.kind,
		Outcome: written.Status, Chunks: written.Chunks, At: s.now().UTC(),
	}); err != nil {
		return result, fmt.Errorf("record owner action: %w", err)
	}
	return result, nil
}

func (s *Service) load(ctx context.Context, source string, isURL bool) (*loadedSource, error) {
	if isURL {
		return loadURL(ctx, s.http, source)
	}
	return loadFile(source)
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

func (s *Service) Remove(ctx context.Context, collection, source string, apply bool) (RemoveOutcome, error) {
	if err := requireCollection(collection); err != nil {
		return RemoveOutcome{}, err
	}
	if source == "" {
		return RemoveOutcome{}, Errorf(ErrorCodeInvalidArgument, "source must not be empty")
	}
	outcome, err := s.store.RemoveSource(ctx, collection, source, apply)
	if err != nil && !outcome.Applied {
		return RemoveOutcome{}, err
	}
	if !apply {
		return outcome, err
	}
	if actionErr := s.actions.RecordOwnerAction(ctx, &OwnerAction{
		Action: ActionRemove, Collection: collection, SourceID: outcome.SourceID,
		Outcome: "removed", Chunks: outcome.Chunks, At: s.now().UTC(),
	}); actionErr != nil {
		return outcome, errors.Join(err, fmt.Errorf("record owner action: %w", actionErr))
	}
	return outcome, err
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

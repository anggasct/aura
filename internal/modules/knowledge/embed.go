package knowledge

import (
	"context"
	"errors"
	"fmt"
	"math"
)

type EmbedRole string

const (
	EmbedRoleDocument EmbedRole = "document"
	EmbedRoleQuery    EmbedRole = "query"

	EmbedOutcomeEmbedded        = "embedded"
	EmbedOutcomeUnavailable     = "unavailable"
	EmbedOutcomeReembedRequired = "reembed_required"

	charactersPerEstimatedToken  = 4
	embedOutcomeCompleted        = "completed"
	embedOutcomePartial          = "partial"
	embedOutcomeOptedOut         = "opted_out"
	embedUnavailableDetail       = "embedding is not configured or its credential could not be resolved"
	embedCollectionMissingDetail = "collection does not exist"
	embedChangedDetail           = "the collection or its opt-in changed while vectors were computed; nothing more was sent, rerun to resume"
)

type Embedder interface {
	Embed(ctx context.Context, role EmbedRole, texts []string) ([][]float32, error)
}

type EmbeddingSettings struct {
	Fingerprint   string
	Destination   string
	Loopback      bool
	Model         string
	Dimensions    int
	BatchSize     int
	MaxMemory     int64
	PublicQueries bool
}

func (e *EmbeddingSettings) valid() bool {
	return e.Fingerprint != "" && e.Destination != "" && e.Dimensions > 0 && e.BatchSize > 0
}

type EmbeddingState struct {
	Exists            bool
	Fingerprint       string
	Chunks            int
	Covered           int
	PendingCharacters int64
}

type PendingChunk struct {
	RowID   int64
	Heading string
	Content string
	SHA256  string
}

type VectorWrite struct {
	RowID         int64
	ContentSHA256 string
	Vector        []float32
}

type CommitOutcome struct {
	Written   int
	Completed bool
}

type DisableOutcome struct {
	OptedIn      bool
	Vectors      int
	Applied      bool
	WALTruncated bool
}

type EmbeddingStore interface {
	EmbeddingState(ctx context.Context, collection, fingerprint string) (EmbeddingState, error)
	StartEmbedding(ctx context.Context, collection, fingerprint string) error
	PendingChunks(ctx context.Context, collection, fingerprint string, afterRowID int64, limit int) ([]PendingChunk, error)
	CommitVectors(ctx context.Context, collection, fingerprint string, vectors []VectorWrite) (CommitOutcome, error)
	DisableEmbedding(ctx context.Context, collection string, apply bool) (DisableOutcome, error)
}

type EmbedOutcome struct {
	State string
	Sent  int
}

type EmbedPreview struct {
	Collection      string
	Destination     string
	Loopback        bool
	Model           string
	Dimensions      int
	Chunks          int
	Pending         int
	EstimatedTokens int64
	OptedIn         bool
	Reembed         bool
	PublicQueries   bool
}

type EmbedResult struct {
	Collection  string
	Destination string
	Sent        int
	Batches     int
	Completed   bool
}

func (s *Service) requireEmbedding() error {
	if s.vectors == nil || s.embedder == nil || s.embedding == nil {
		return Errorf(ErrorCodeEmbeddingUnavailable, "%s", embedUnavailableDetail)
	}
	return nil
}

func (s *Service) embeddingFingerprint() string {
	if s.embedding == nil {
		return ""
	}
	return s.embedding.Fingerprint
}

func (s *Service) EmbedPreview(ctx context.Context, collection string) (EmbedPreview, error) {
	if err := requireCollection(collection); err != nil {
		return EmbedPreview{}, err
	}
	if err := s.requireEmbedding(); err != nil {
		return EmbedPreview{}, err
	}
	state, err := s.vectors.EmbeddingState(ctx, collection, s.embedding.Fingerprint)
	if err != nil {
		return EmbedPreview{}, err
	}
	if !state.Exists {
		return EmbedPreview{}, Errorf(ErrorCodeSourceNotFound, "%s", embedCollectionMissingDetail)
	}
	return EmbedPreview{
		Collection:      collection,
		Destination:     s.embedding.Destination,
		Loopback:        s.embedding.Loopback,
		Model:           s.embedding.Model,
		Dimensions:      s.embedding.Dimensions,
		Chunks:          state.Chunks,
		Pending:         state.Chunks - state.Covered,
		EstimatedTokens: (state.PendingCharacters + charactersPerEstimatedToken - 1) / charactersPerEstimatedToken,
		OptedIn:         state.Fingerprint != "",
		Reembed:         state.Fingerprint != "" && state.Fingerprint != s.embedding.Fingerprint,
		PublicQueries:   s.embedding.PublicQueries,
	}, nil
}

func (s *Service) Embed(ctx context.Context, collection string) (EmbedResult, error) {
	if err := requireCollection(collection); err != nil {
		return EmbedResult{}, err
	}
	if err := s.requireEmbedding(); err != nil {
		return EmbedResult{}, err
	}
	state, err := s.vectors.EmbeddingState(ctx, collection, s.embedding.Fingerprint)
	if err != nil {
		return EmbedResult{}, err
	}
	if !state.Exists {
		return EmbedResult{}, Errorf(ErrorCodeSourceNotFound, "%s", embedCollectionMissingDetail)
	}
	if err := s.vectors.StartEmbedding(ctx, collection, s.embedding.Fingerprint); err != nil {
		return EmbedResult{}, err
	}
	result, runErr := s.embedPending(ctx, collection)
	outcome := embedOutcomeCompleted
	if runErr != nil || !result.Completed {
		outcome = embedOutcomePartial
	}
	if actionErr := s.actions.RecordOwnerAction(ctx, &OwnerAction{
		Action: ActionEmbed, Collection: collection, Outcome: outcome, Chunks: result.Sent, At: s.now().UTC(),
	}); actionErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("record owner action: %w", actionErr))
	}
	return result, runErr
}

func (s *Service) EmbedOff(ctx context.Context, collection string, apply bool) (DisableOutcome, error) {
	if err := requireCollection(collection); err != nil {
		return DisableOutcome{}, err
	}
	if s.vectors == nil {
		return DisableOutcome{}, Errorf(ErrorCodeEmbeddingUnavailable, "%s", embedUnavailableDetail)
	}
	outcome, err := s.vectors.DisableEmbedding(ctx, collection, apply)
	if err != nil && !outcome.Applied {
		return DisableOutcome{}, err
	}
	if !apply {
		return outcome, err
	}
	if actionErr := s.actions.RecordOwnerAction(ctx, &OwnerAction{
		Action: ActionEmbedOff, Collection: collection, Outcome: embedOutcomeOptedOut, Chunks: outcome.Vectors, At: s.now().UTC(),
	}); actionErr != nil {
		return outcome, errors.Join(err, fmt.Errorf("record owner action: %w", actionErr))
	}
	return outcome, err
}

func (s *Service) embedAfterWrite(ctx context.Context, collection string) (EmbedOutcome, error) {
	if s.vectors == nil {
		return EmbedOutcome{}, nil
	}
	state, err := s.vectors.EmbeddingState(ctx, collection, s.embeddingFingerprint())
	if err != nil {
		return EmbedOutcome{}, err
	}
	if state.Fingerprint == "" {
		return EmbedOutcome{}, nil
	}
	if s.embedding == nil || s.embedder == nil {
		return EmbedOutcome{State: EmbedOutcomeUnavailable}, nil
	}
	if state.Fingerprint != s.embedding.Fingerprint {
		return EmbedOutcome{State: EmbedOutcomeReembedRequired}, nil
	}
	result, err := s.embedPending(ctx, collection)
	return EmbedOutcome{State: EmbedOutcomeEmbedded, Sent: result.Sent}, err
}

func (s *Service) embedPending(ctx context.Context, collection string) (EmbedResult, error) {
	result := EmbedResult{Collection: collection, Destination: s.embedding.Destination, Completed: true}
	var after int64
	for {
		if err := s.requireSendGate(ctx, collection); err != nil {
			result.Completed = false
			return result, err
		}
		chunks, err := s.vectors.PendingChunks(ctx, collection, s.embedding.Fingerprint, after, s.embedding.BatchSize)
		if err != nil {
			result.Completed = false
			return result, err
		}
		if len(chunks) == 0 {
			result.Completed = true
			return result, nil
		}
		committed, err := s.embedBatch(ctx, collection, chunks)
		if err != nil {
			result.Completed = false
			return result, err
		}
		result.Sent += committed.Written
		result.Batches++
		result.Completed = committed.Completed
		after = chunks[len(chunks)-1].RowID
	}
}

func (s *Service) requireSendGate(ctx context.Context, collection string) error {
	state, err := s.vectors.EmbeddingState(ctx, collection, s.embedding.Fingerprint)
	if err != nil {
		return err
	}
	if !state.Exists || state.Fingerprint != s.embedding.Fingerprint {
		return Errorf(ErrorCodeEmbeddingFailed, "%s", embedChangedDetail)
	}
	return nil
}

func (s *Service) embedBatch(ctx context.Context, collection string, chunks []PendingChunk) (CommitOutcome, error) {
	texts := make([]string, len(chunks))
	for i := range chunks {
		texts[i] = embeddingText(&chunks[i])
	}
	if err := s.screenBeforeSend(ctx, texts); err != nil {
		return CommitOutcome{}, err
	}
	vectors, err := s.embedder.Embed(ctx, EmbedRoleDocument, texts)
	if err != nil {
		return CommitOutcome{}, embeddingFailure(err)
	}
	if len(vectors) != len(chunks) {
		return CommitOutcome{}, Errorf(ErrorCodeEmbeddingFailed, "the provider returned %d vectors for %d chunks", len(vectors), len(chunks))
	}
	writes := make([]VectorWrite, len(chunks))
	for i := range chunks {
		unit, ok := normalizeVector(vectors[i], s.embedding.Dimensions)
		if !ok {
			return CommitOutcome{}, Errorf(ErrorCodeEmbeddingFailed, "the provider returned a vector that cannot be stored")
		}
		writes[i] = VectorWrite{RowID: chunks[i].RowID, ContentSHA256: chunks[i].SHA256, Vector: unit}
	}
	return s.vectors.CommitVectors(ctx, collection, s.embedding.Fingerprint, writes)
}

func (s *Service) screenBeforeSend(ctx context.Context, texts []string) error {
	screen, err := s.secrets.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", &Error{Code: ErrorCodeSecretUnavailable, Detail: "a configured secret reference could not be resolved"}, err)
	}
	for _, text := range texts {
		if err := screenText(text, screen); err != nil {
			return err
		}
	}
	return nil
}

func embeddingText(chunk *PendingChunk) string {
	if chunk.Heading == "" {
		return chunk.Content
	}
	return chunk.Heading + "\n" + chunk.Content
}

func embeddingFailure(err error) error {
	if _, coded := CodeOf(err); coded {
		return err
	}
	return fmt.Errorf("%w: %w", &Error{Code: ErrorCodeEmbeddingFailed, Detail: "the embedding provider call failed"}, err)
}

func normalizeVector(vector []float32, dimensions int) ([]float32, bool) {
	if len(vector) != dimensions {
		return nil, false
	}
	var sum float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, false
		}
		sum += float64(value) * float64(value)
	}
	norm := math.Sqrt(sum)
	if norm == 0 || math.IsInf(norm, 0) {
		return nil, false
	}
	unit := make([]float32, len(vector))
	for i, value := range vector {
		unit[i] = float32(float64(value) / norm)
	}
	return unit, true
}

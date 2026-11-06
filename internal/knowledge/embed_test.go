package knowledge

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
)

type fakeVectorChunk struct {
	rowID   int64
	heading string
	content string
	sha     string
}

type fakeVectorStore struct {
	exists      bool
	stored      string
	chunks      []fakeVectorChunk
	vectors     map[int64]VectorWrite
	vectorPrint map[int64]string
	generation  int
	commits     int
	failCommit  error
	onCommit    func()
}

func newFakeVectorStore(contents ...string) *fakeVectorStore {
	store := &fakeVectorStore{exists: true, vectors: map[int64]VectorWrite{}, vectorPrint: map[int64]string{}}
	for i, content := range contents {
		store.chunks = append(store.chunks, fakeVectorChunk{rowID: int64(i + 1), heading: "Section", content: content, sha: strings.Repeat(string(rune('a'+i)), 64)})
	}
	return store
}

func (s *fakeVectorStore) covered(fingerprint string, chunk *fakeVectorChunk) bool {
	vector, ok := s.vectors[chunk.rowID]
	return ok && s.vectorPrint[chunk.rowID] == fingerprint && vector.ContentSHA256 == chunk.sha
}

func (s *fakeVectorStore) EmbeddingState(_ context.Context, _, fingerprint string) (EmbeddingState, error) {
	if !s.exists {
		return EmbeddingState{}, nil
	}
	state := EmbeddingState{Exists: true, Fingerprint: s.stored, Chunks: len(s.chunks)}
	for i := range s.chunks {
		if s.covered(fingerprint, &s.chunks[i]) {
			state.Covered++
			continue
		}
		state.PendingCharacters += int64(len(s.chunks[i].heading) + len(s.chunks[i].content))
	}
	return state, nil
}

func (s *fakeVectorStore) StartEmbedding(_ context.Context, _, fingerprint string) error {
	s.stored = fingerprint
	return nil
}

func (s *fakeVectorStore) PendingChunks(_ context.Context, _, fingerprint string, after int64, limit int) ([]PendingChunk, error) {
	var pending []PendingChunk
	for i := range s.chunks {
		chunk := &s.chunks[i]
		if chunk.rowID <= after || s.covered(fingerprint, chunk) {
			continue
		}
		pending = append(pending, PendingChunk{RowID: chunk.rowID, Heading: chunk.heading, Content: chunk.content, SHA256: chunk.sha})
		if len(pending) == limit {
			break
		}
	}
	return pending, nil
}

func (s *fakeVectorStore) CommitVectors(_ context.Context, _, fingerprint string, vectors []VectorWrite) (CommitOutcome, error) {
	if s.failCommit != nil {
		return CommitOutcome{}, s.failCommit
	}
	if s.stored != fingerprint {
		return CommitOutcome{}, Errorf(ErrorCodeEmbeddingFailed, "opt-in changed")
	}
	for _, vector := range vectors {
		s.vectors[vector.RowID] = vector
		s.vectorPrint[vector.RowID] = fingerprint
	}
	s.commits++
	if s.onCommit != nil {
		defer s.onCommit()
	}
	for i := range s.chunks {
		if !s.covered(fingerprint, &s.chunks[i]) {
			return CommitOutcome{Written: len(vectors)}, nil
		}
	}
	s.generation++
	return CommitOutcome{Written: len(vectors), Completed: true}, nil
}

func (s *fakeVectorStore) DisableEmbedding(_ context.Context, _ string, apply bool) (DisableOutcome, error) {
	outcome := DisableOutcome{OptedIn: s.stored != "", Vectors: len(s.vectors), Applied: apply, WALTruncated: apply}
	if apply {
		s.stored = ""
		s.vectors = map[int64]VectorWrite{}
		s.vectorPrint = map[int64]string{}
		s.generation++
	}
	return outcome, nil
}

type fakeEmbedder struct {
	calls   [][]string
	roles   []EmbedRole
	failAt  int
	failErr error
	reply   func(texts []string) [][]float32
	onCall  func()
}

func (e *fakeEmbedder) Embed(_ context.Context, role EmbedRole, texts []string) ([][]float32, error) {
	e.calls = append(e.calls, texts)
	e.roles = append(e.roles, role)
	if e.onCall != nil {
		e.onCall()
	}
	if e.failAt > 0 && len(e.calls) == e.failAt {
		return nil, e.failErr
	}
	if e.reply != nil {
		return e.reply(texts), nil
	}
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{3, 4, 0}
	}
	return vectors, nil
}

func (e *fakeEmbedder) sent() int {
	total := 0
	for _, call := range e.calls {
		total += len(call)
	}
	return total
}

const testFingerprint = "fingerprint-current"

type embedFixture struct {
	*serviceFixture
	vectors  *fakeVectorStore
	embedder *fakeEmbedder
}

func newEmbedFixture(t *testing.T, batchSize int, contents ...string) *embedFixture {
	t.Helper()
	base := newFixture(t)
	fixture := &embedFixture{serviceFixture: base, vectors: newFakeVectorStore(contents...), embedder: &fakeEmbedder{}}
	fixture.rebuild(t, &EmbeddingSettings{
		Fingerprint: testFingerprint, Destination: "api.example.com:443", Model: "test-model", Dimensions: 3, BatchSize: batchSize,
	})
	return fixture
}

func (f *embedFixture) rebuild(t *testing.T, settings *EmbeddingSettings) {
	t.Helper()
	service, err := NewService(&Deps{
		Store: f.store, Secrets: f.secrets, Reducer: func(context.Context, []byte, int64) (string, bool, error) { return "", false, nil },
		HTTP: f.doer, Actions: f.actions, Vectors: f.vectors, Embedder: f.embedder, Embedding: settings, Now: f.service.now,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	f.service = service
}

func TestEmbedPreviewReportsDestinationAndSendsNothing(t *testing.T) {
	fixture := newEmbedFixture(t, 2, "alpha text", "beta text")
	preview, err := fixture.service.EmbedPreview(t.Context(), "support")
	if err != nil {
		t.Fatalf("EmbedPreview: %v", err)
	}
	want := EmbedPreview{
		Collection: "support", Destination: "api.example.com:443", Model: "test-model", Dimensions: 3,
		Chunks: 2, Pending: 2, EstimatedTokens: 9,
	}
	if preview != want {
		t.Fatalf("preview = %+v, want %+v", preview, want)
	}
	if len(fixture.embedder.calls) != 0 || fixture.vectors.stored != "" || len(fixture.vectors.vectors) != 0 || len(fixture.actions.actions) != 0 {
		t.Fatalf("the preview changed state or sent text: calls %d stored %q vectors %d actions %d",
			len(fixture.embedder.calls), fixture.vectors.stored, len(fixture.vectors.vectors), len(fixture.actions.actions))
	}
}

func TestEmbedPreviewFlagsAReembedAndPublicQueries(t *testing.T) {
	fixture := newEmbedFixture(t, 2, "alpha text")
	fixture.vectors.stored = "fingerprint-old"
	fixture.rebuild(t, &EmbeddingSettings{Fingerprint: testFingerprint, Destination: "127.0.0.1:11434", Loopback: true, Model: "m", Dimensions: 3, BatchSize: 2, PublicQueries: true})
	preview, err := fixture.service.EmbedPreview(t.Context(), "support")
	if err != nil || !preview.Reembed || !preview.OptedIn || !preview.Loopback || !preview.PublicQueries {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	if len(fixture.embedder.calls) != 0 {
		t.Fatal("the preview sent text")
	}
}

func TestEmbedWithoutConfigurationFailsAndSendsNothing(t *testing.T) {
	fixture := newEmbedFixture(t, 2, "alpha text")
	fixture.rebuild(t, nil)
	_, err := fixture.service.Embed(t.Context(), "support")
	requireCode(t, err, ErrorCodeEmbeddingUnavailable)
	_, err = fixture.service.EmbedPreview(t.Context(), "support")
	requireCode(t, err, ErrorCodeEmbeddingUnavailable)
	if len(fixture.embedder.calls) != 0 || fixture.vectors.stored != "" {
		t.Fatal("an unconfigured run sent text or recorded an opt-in")
	}
}

func TestEmbedWithoutACredentialFailsAndSendsNothing(t *testing.T) {
	fixture := newEmbedFixture(t, 2, "alpha text")
	service, err := NewService(&Deps{
		Store: fixture.store, Secrets: fixture.secrets, Reducer: func(context.Context, []byte, int64) (string, bool, error) { return "", false, nil },
		HTTP: fixture.doer, Actions: fixture.actions, Vectors: fixture.vectors,
		Embedding: &EmbeddingSettings{Fingerprint: testFingerprint, Destination: "x:443", Dimensions: 3, BatchSize: 2},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = service.Embed(t.Context(), "support")
	requireCode(t, err, ErrorCodeEmbeddingUnavailable)
	if fixture.vectors.stored != "" {
		t.Fatal("an opt-in was recorded without a usable embedder")
	}
}

func TestEmbedRejectsInvalidAndMissingCollections(t *testing.T) {
	fixture := newEmbedFixture(t, 2, "alpha text")
	_, err := fixture.service.Embed(t.Context(), "Bad Name")
	requireCode(t, err, ErrorCodeCollectionInvalid)
	_, err = fixture.service.EmbedPreview(t.Context(), "Bad Name")
	requireCode(t, err, ErrorCodeCollectionInvalid)
	_, err = fixture.service.EmbedOff(t.Context(), "Bad Name", true)
	requireCode(t, err, ErrorCodeCollectionInvalid)
	fixture.vectors.exists = false
	_, err = fixture.service.Embed(t.Context(), "support")
	requireCode(t, err, ErrorCodeSourceNotFound)
	_, err = fixture.service.EmbedPreview(t.Context(), "support")
	requireCode(t, err, ErrorCodeSourceNotFound)
	if len(fixture.embedder.calls) != 0 {
		t.Fatal("text was sent for a missing collection")
	}
}

func TestEmbedSendsBatchesAndStoresUnitVectors(t *testing.T) {
	fixture := newEmbedFixture(t, 2, "alpha text", "beta text", "gamma text")
	result, err := fixture.service.Embed(t.Context(), "support")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if result.Sent != 3 || result.Batches != 2 || !result.Completed || result.Destination != "api.example.com:443" {
		t.Fatalf("result = %+v", result)
	}
	if fixture.vectors.stored != testFingerprint || fixture.vectors.generation != 1 || fixture.vectors.commits != 2 {
		t.Fatalf("store = stored %q generation %d commits %d, want one bump over two commits", fixture.vectors.stored, fixture.vectors.generation, fixture.vectors.commits)
	}
	if len(fixture.embedder.calls) != 2 || len(fixture.embedder.calls[0]) != 2 || fixture.embedder.calls[0][0] != "Section\nalpha text" {
		t.Fatalf("calls = %v", fixture.embedder.calls)
	}
	for _, role := range fixture.embedder.roles {
		if role != EmbedRoleDocument {
			t.Errorf("role = %q, want document", role)
		}
	}
	for rowID, vector := range fixture.vectors.vectors {
		var length float64
		for _, value := range vector.Vector {
			length += float64(value) * float64(value)
		}
		if math.Abs(math.Sqrt(length)-1) > 1e-6 || vector.Vector[0] != 0.6 {
			t.Errorf("vector %d = %v, want the unit vector of (3,4,0)", rowID, vector.Vector)
		}
	}
	if len(fixture.actions.actions) != 1 || fixture.actions.actions[0].Action != ActionEmbed || fixture.actions.actions[0].Outcome != "completed" || fixture.actions.actions[0].Chunks != 3 {
		t.Fatalf("owner actions = %+v", fixture.actions.actions)
	}
	if fixture.secrets.calls != 2 {
		t.Errorf("secret snapshots = %d, want one per batch", fixture.secrets.calls)
	}
}

func TestEmbedResumesAfterAFailureAndSkipsMatchingVectors(t *testing.T) {
	fixture := newEmbedFixture(t, 2, "alpha text", "beta text", "gamma text")
	fixture.embedder.failAt = 2
	fixture.embedder.failErr = errors.New("provider unavailable")
	result, err := fixture.service.Embed(t.Context(), "support")
	requireCode(t, err, ErrorCodeEmbeddingFailed)
	if result.Sent != 2 || result.Completed || len(fixture.vectors.vectors) != 2 || fixture.vectors.generation != 0 {
		t.Fatalf("after failure: result %+v vectors %d generation %d", result, len(fixture.vectors.vectors), fixture.vectors.generation)
	}
	if fixture.actions.actions[0].Outcome != "partial" {
		t.Errorf("owner action = %+v, want partial", fixture.actions.actions[0])
	}

	fixture.embedder.calls = nil
	fixture.embedder.failAt = 0
	result, err = fixture.service.Embed(t.Context(), "support")
	if err != nil || result.Sent != 1 || !result.Completed {
		t.Fatalf("resume = %+v, %v", result, err)
	}
	if fixture.embedder.sent() != 1 || fixture.embedder.calls[0][0] != "Section\ngamma text" {
		t.Fatalf("resumed run sent %v, want only the missing chunk", fixture.embedder.calls)
	}
	if fixture.vectors.generation != 1 {
		t.Errorf("generation = %d, want one bump for the whole run", fixture.vectors.generation)
	}
}

func TestEmbedRerunWithFullCoverageSendsNothing(t *testing.T) {
	fixture := newEmbedFixture(t, 2, "alpha text", "beta text")
	if _, err := fixture.service.Embed(t.Context(), "support"); err != nil {
		t.Fatalf("first run: %v", err)
	}
	fixture.embedder.calls = nil
	result, err := fixture.service.Embed(t.Context(), "support")
	if err != nil || result.Sent != 0 || !result.Completed || len(fixture.embedder.calls) != 0 {
		t.Fatalf("second run = %+v, %v, calls %d", result, err, len(fixture.embedder.calls))
	}
	if fixture.vectors.generation != 1 {
		t.Errorf("generation = %d, want no further bump", fixture.vectors.generation)
	}
}

func TestEmbedStoresNothingForAMalformedProviderBatch(t *testing.T) {
	tests := map[string]func(texts []string) [][]float32{
		"too few vectors": func(texts []string) [][]float32 { return [][]float32{{1, 0, 0}}[:len(texts)-1] },
		"too many":        func(texts []string) [][]float32 { return append(unitReply(texts), []float32{1, 0, 0}) },
		"short vector":    func(texts []string) [][]float32 { r := unitReply(texts); r[1] = []float32{1, 0}; return r },
		"long vector":     func(texts []string) [][]float32 { r := unitReply(texts); r[1] = []float32{1, 0, 0, 0}; return r },
		"nan":             func(texts []string) [][]float32 { r := unitReply(texts); r[1][0] = float32(math.NaN()); return r },
		"infinite":        func(texts []string) [][]float32 { r := unitReply(texts); r[0][2] = float32(math.Inf(1)); return r },
		"all zero":        func(texts []string) [][]float32 { r := unitReply(texts); r[1] = []float32{0, 0, 0}; return r },
	}
	for name, reply := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newEmbedFixture(t, 2, "alpha text", "beta text")
			fixture.embedder.reply = reply
			_, err := fixture.service.Embed(t.Context(), "support")
			requireCode(t, err, ErrorCodeEmbeddingFailed)
			if len(fixture.vectors.vectors) != 0 || fixture.vectors.commits != 0 {
				t.Fatalf("vectors = %d commits = %d, want nothing stored for the batch", len(fixture.vectors.vectors), fixture.vectors.commits)
			}
		})
	}
}

func unitReply(texts []string) [][]float32 {
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{1, 0, 0}
	}
	return vectors
}

func TestEmbedScreensChunksBeforeSendingAndStopsOnAMatch(t *testing.T) {
	tests := map[string]string{
		"vault value":        "The key is vault-secret-value here.",
		"credential pattern": "token ghp_" + strings.Repeat("a", 36),
	}
	for name, hostile := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newEmbedFixture(t, 1, "alpha text", hostile)
			result, err := fixture.service.Embed(t.Context(), "support")
			requireCode(t, err, ErrorCodeSecretInvalid)
			if fixture.embedder.sent() != 1 || strings.Contains(strings.Join(fixture.embedder.calls[0], ""), "vault-secret-value") {
				t.Fatalf("sent %v; only the clean chunk may leave", fixture.embedder.calls)
			}
			if result.Completed || len(fixture.vectors.vectors) != 1 {
				t.Errorf("result %+v vectors %d", result, len(fixture.vectors.vectors))
			}
		})
	}
}

func TestEmbedScreensAgainstCurrentSecretsNotTheIngestSnapshot(t *testing.T) {
	fixture := newEmbedFixture(t, 5, "A paragraph carrying rotated-secret-value inside.")
	fixture.secrets.screen = fakeVault{values: []string{"rotated-secret-value"}}
	_, err := fixture.service.Embed(t.Context(), "support")
	requireCode(t, err, ErrorCodeSecretInvalid)
	if len(fixture.embedder.calls) != 0 {
		t.Fatal("a chunk matching a current vault value was sent")
	}
}

func TestEmbedSendsNothingWhenSecretsCannotBeResolved(t *testing.T) {
	fixture := newEmbedFixture(t, 5, "alpha text")
	fixture.secrets.err = errors.New("unresolved")
	_, err := fixture.service.Embed(t.Context(), "support")
	requireCode(t, err, ErrorCodeSecretUnavailable)
	if len(fixture.embedder.calls) != 0 {
		t.Fatal("text was sent without a secret screen")
	}
}

func TestEmbedStopsWhenTheOptInIsClearedDuringTheRun(t *testing.T) {
	fixture := newEmbedFixture(t, 1, "alpha text", "beta text")
	fixture.embedder.onCall = func() {
		if _, err := fixture.service.EmbedOff(t.Context(), "support", true); err != nil {
			t.Errorf("EmbedOff: %v", err)
		}
	}
	_, err := fixture.service.Embed(t.Context(), "support")
	requireCode(t, err, ErrorCodeEmbeddingFailed)
	if len(fixture.embedder.calls) != 1 || len(fixture.vectors.vectors) != 0 || fixture.vectors.stored != "" {
		t.Fatalf("calls %d vectors %d stored %q; the run must stop after the cleared opt-in", len(fixture.embedder.calls), len(fixture.vectors.vectors), fixture.vectors.stored)
	}
}

func TestEmbedStopsOnAStoreRejection(t *testing.T) {
	fixture := newEmbedFixture(t, 1, "alpha text", "beta text")
	fixture.vectors.failCommit = Errorf(ErrorCodeEmbeddingFailed, "chunk changed")
	_, err := fixture.service.Embed(t.Context(), "support")
	requireCode(t, err, ErrorCodeEmbeddingFailed)
	if len(fixture.embedder.calls) != 1 {
		t.Fatalf("calls = %d, want the run to stop at the first rejected commit", len(fixture.embedder.calls))
	}
}

func TestEmbedOffNeedsNoConfigurationAndRecordsTheAction(t *testing.T) {
	fixture := newEmbedFixture(t, 2, "alpha text")
	if _, err := fixture.service.Embed(t.Context(), "support"); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	fixture.rebuild(t, nil)
	fixture.actions.actions = nil

	preview, err := fixture.service.EmbedOff(t.Context(), "support", false)
	if err != nil || preview.Applied || preview.Vectors != 1 || !preview.OptedIn {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	if len(fixture.vectors.vectors) != 1 || len(fixture.actions.actions) != 0 {
		t.Fatal("the preview changed state")
	}
	outcome, err := fixture.service.EmbedOff(t.Context(), "support", true)
	if err != nil || !outcome.Applied || !outcome.WALTruncated || outcome.Vectors != 1 {
		t.Fatalf("EmbedOff = %+v, %v", outcome, err)
	}
	if len(fixture.vectors.vectors) != 0 || fixture.vectors.stored != "" {
		t.Fatal("vectors or the opt-in remain")
	}
	if len(fixture.actions.actions) != 1 || fixture.actions.actions[0].Action != ActionEmbedOff {
		t.Fatalf("owner actions = %+v", fixture.actions.actions)
	}
}

func TestAddEmbedsChangedChunksOnlyForAnOptedInCollection(t *testing.T) {
	path := writeSource(t, "Policy.md", []byte("# Refunds\nRefunds take ten days.\n"))

	t.Run("not opted in", func(t *testing.T) {
		fixture := newEmbedFixture(t, 5, "alpha text")
		result, err := fixture.service.Add(t.Context(), "support", path)
		if err != nil || result.Embedding != (EmbedOutcome{}) || len(fixture.embedder.calls) != 0 {
			t.Fatalf("Add = %+v, %v, calls %d; want text only", result, err, len(fixture.embedder.calls))
		}
	})

	t.Run("opted in with the current fingerprint", func(t *testing.T) {
		fixture := newEmbedFixture(t, 5, "alpha text")
		fixture.vectors.stored = testFingerprint
		result, err := fixture.service.Add(t.Context(), "support", path)
		if err != nil || result.Embedding.State != EmbedOutcomeEmbedded || result.Embedding.Sent != 1 || fixture.embedder.sent() != 1 {
			t.Fatalf("Add = %+v, %v, sent %d", result, err, fixture.embedder.sent())
		}
	})

	t.Run("opted in under another fingerprint", func(t *testing.T) {
		fixture := newEmbedFixture(t, 5, "alpha text")
		fixture.vectors.stored = "fingerprint-old"
		result, err := fixture.service.Add(t.Context(), "support", path)
		if err != nil || result.Embedding.State != EmbedOutcomeReembedRequired || len(fixture.embedder.calls) != 0 {
			t.Fatalf("Add = %+v, %v, calls %d; want nothing sent", result, err, len(fixture.embedder.calls))
		}
		if len(fixture.store.writes) != 1 {
			t.Errorf("text writes = %d, want the text committed", len(fixture.store.writes))
		}
	})

	t.Run("opted in without configuration", func(t *testing.T) {
		fixture := newEmbedFixture(t, 5, "alpha text")
		fixture.vectors.stored = testFingerprint
		fixture.rebuild(t, nil)
		result, err := fixture.service.Add(t.Context(), "support", path)
		if err != nil || result.Embedding.State != EmbedOutcomeUnavailable || len(fixture.embedder.calls) != 0 {
			t.Fatalf("Add = %+v, %v, calls %d; want nothing sent", result, err, len(fixture.embedder.calls))
		}
	})

	t.Run("embedding failure keeps the text", func(t *testing.T) {
		fixture := newEmbedFixture(t, 5, "alpha text")
		fixture.vectors.stored = testFingerprint
		fixture.embedder.failAt = 1
		fixture.embedder.failErr = errors.New("provider unavailable")
		result, err := fixture.service.Add(t.Context(), "support", path)
		requireCode(t, err, ErrorCodeEmbeddingFailed)
		if result.SourceID == "" || len(fixture.store.writes) != 1 {
			t.Fatalf("result %+v writes %d; the text must stay committed", result, len(fixture.store.writes))
		}
	})

	t.Run("an unchanged source sends nothing", func(t *testing.T) {
		fixture := newEmbedFixture(t, 5, "alpha text")
		fixture.vectors.stored = testFingerprint
		fixture.store.unchanged = true
		result, err := fixture.service.Add(t.Context(), "support", path)
		if err != nil || result.Embedding != (EmbedOutcome{}) || len(fixture.embedder.calls) != 0 {
			t.Fatalf("Add = %+v, %v, calls %d", result, err, len(fixture.embedder.calls))
		}
	})
}

func TestRefreshEmbedsOnceAfterAllSourcesForAnOptedInCollection(t *testing.T) {
	fixture := newEmbedFixture(t, 5, "alpha text", "beta text")
	fixture.vectors.stored = testFingerprint
	first := writeSource(t, "a.md", []byte("alpha"))
	second := writeSource(t, "b.md", []byte("beta"))
	fixture.store.sources["support"] = []SourceRecord{
		{Collection: "support", Kind: SourceKindFile, URI: first, Label: "a.md"},
		{Collection: "support", Kind: SourceKindFile, URI: second, Label: "b.md"},
	}
	results, err := fixture.service.Refresh(t.Context(), "support", "")
	if err != nil || len(results) != 2 {
		t.Fatalf("Refresh = %+v, %v", results, err)
	}
	if len(fixture.embedder.calls) != 1 || fixture.embedder.sent() != 2 {
		t.Fatalf("embedder calls = %d sent = %d, want one batch after both sources", len(fixture.embedder.calls), fixture.embedder.sent())
	}
	for i := range results {
		if results[i].Embedding.State != EmbedOutcomeEmbedded || results[i].Embedding.Sent != 2 {
			t.Errorf("result %d embedding = %+v", i, results[i].Embedding)
		}
	}
}

func TestRefreshOfUnchangedSourcesSendsNothing(t *testing.T) {
	fixture := newEmbedFixture(t, 5, "alpha text")
	fixture.vectors.stored = testFingerprint
	fixture.store.unchanged = true
	path := writeSource(t, "a.md", []byte("alpha"))
	fixture.store.sources["support"] = []SourceRecord{{Collection: "support", Kind: SourceKindFile, URI: path, Label: "a.md"}}
	if _, err := fixture.service.Refresh(t.Context(), "support", ""); err != nil || len(fixture.embedder.calls) != 0 {
		t.Fatalf("Refresh = %v, calls %d", err, len(fixture.embedder.calls))
	}
}

func TestEmbedSendsNoFurtherBatchOnceTheOptInChanged(t *testing.T) {
	for name, change := range map[string]func(*fakeVectorStore){
		"cleared":             func(s *fakeVectorStore) { s.stored = "" },
		"another fingerprint": func(s *fakeVectorStore) { s.stored = "fingerprint-other" },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newEmbedFixture(t, 1, "alpha text", "beta text", "gamma text")
			fixture.vectors.onCommit = func() { change(fixture.vectors) }
			result, err := fixture.service.Embed(t.Context(), "support")
			requireCode(t, err, ErrorCodeEmbeddingFailed)
			if len(fixture.embedder.calls) != 1 || result.Completed || result.Sent != 1 {
				t.Fatalf("calls %d result %+v; the second batch must not leave the host", len(fixture.embedder.calls), result)
			}
		})
	}
}

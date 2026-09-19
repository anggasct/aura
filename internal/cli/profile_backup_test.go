package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"

	"github.com/anggasct/aura/internal/profile"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/telemetry"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestProfileBackupRestorePreservesLifecycle(t *testing.T) {
	cfgPath := writeProfileCLIConfig(t)
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	now := time.Now().UTC()
	sessions := store.NewSessionService(db)
	if err := sessions.Create(t.Context(), &store.Session{ID: "sess-bk", OwnerID: profileLocalOwner, CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	events := store.NewEventStore(db)
	for i, text := range []string{"i write backends in Go", "also rust sometimes", "my editor is neovim"} {
		if _, err := events.AppendSequenced(t.Context(), "sess-bk", &store.RuntimeEvent{
			ID: fmt.Sprintf("evt-bk-%d", i+1), SessionID: "sess-bk", TurnID: "t1", InvocationID: "inv-1",
			Author: profileLocalOwner, Kind: "message.completed", SchemaVersion: 1,
			Payload: []byte(`{"text":"` + text + `"}`), CreatedAt: now,
		}); err != nil {
			t.Fatalf("AppendSequenced: %v", err)
		}
	}
	service, err := profile.NewService(newProfileRegistry(db), profile.Config{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	evidence := func(eventID, text string) *profile.Evidence {
		return &profile.Evidence{
			SourceEventID: eventID, SourceDigest: profile.ValueDigest(text),
			Provider: "test", Model: "m1", PromptVersion: "v1", ObservedAt: now,
		}
	}
	accepted, err := service.ProposeFact(t.Context(), profileLocalOwner, "language", "backend", "Go", evidence("evt-bk-1", "i write backends in Go"), now)
	if err != nil {
		t.Fatalf("ProposeFact go: %v", err)
	}
	if _, err := service.AcceptFact(t.Context(), profileLocalOwner, accepted.ID, now); err != nil {
		t.Fatalf("AcceptFact: %v", err)
	}
	rival, err := service.ProposeFact(t.Context(), profileLocalOwner, "language", "backend", "Rust", evidence("evt-bk-2", "also rust sometimes"), now)
	if err != nil {
		t.Fatalf("ProposeFact rust: %v", err)
	}
	if err := service.RejectFact(t.Context(), profileLocalOwner, rival.ID, now); err != nil {
		t.Fatalf("RejectFact: %v", err)
	}
	tool, err := service.SetFact(t.Context(), profileLocalOwner, "tool", "editor", "neovim", nil, now)
	if err != nil {
		t.Fatalf("SetFact: %v", err)
	}
	expiring, err := service.SetFact(t.Context(), profileLocalOwner, "timezone", "home", "WIB", ptrTime(now.Add(time.Minute)), now)
	if err != nil {
		t.Fatalf("SetFact expiring: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE profile_fact SET expires_at = ? WHERE id = ?`, now.Add(-time.Hour).Format(time.RFC3339Nano), expiring.ID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}
	if _, err := service.ExpireFacts(t.Context(), now); err != nil {
		t.Fatalf("ExpireFacts: %v", err)
	}
	if err := service.DeleteFact(t.Context(), profileLocalOwner, tool.ID, now); err != nil {
		t.Fatalf("DeleteFact: %v", err)
	}
	retrieved, err := service.Retrieve(t.Context(), &profile.RetrieveQuery{OwnerID: profileLocalOwner, MaxFacts: 10, MaxTokens: 1024}, now)
	if err != nil {
		t.Fatalf("Retrieve before backup: %v", err)
	}
	if len(retrieved.Facts) != 1 || retrieved.Facts[0].Value != "Go" {
		t.Fatalf("retrieved before backup = %+v", retrieved.Facts)
	}
	statuses := map[string]string{}
	for _, fact := range mustListFacts(t, service) {
		statuses[fact.Value] = fact.Status
	}
	if statuses["Go"] != profile.StatusActive || statuses["Rust"] != profile.StatusRejected || statuses["neovim"] != profile.StatusDeleted || statuses["WIB"] != profile.StatusExpired {
		t.Fatalf("seeded statuses = %v", statuses)
	}

	goBefore, err := service.GetFact(t.Context(), profileLocalOwner, accepted.ID)
	if err != nil {
		t.Fatalf("GetFact before backup: %v", err)
	}
	goEvidenceBefore, err := service.EvidenceFor(t.Context(), accepted.ID)
	if err != nil {
		t.Fatalf("EvidenceFor before backup: %v", err)
	}

	dbPath, artifactRoot, _, err := storagePaths(loaded.Config)
	if err != nil {
		t.Fatalf("storagePaths: %v", err)
	}
	backupDir := filepath.Join(t.TempDir(), "backup")
	if _, err := store.Backup(t.Context(), db, backupDir); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := store.VerifyRestore(t.Context(), backupDir, artifactRoot); err != nil {
		t.Fatalf("VerifyRestore: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, err := store.Restore(t.Context(), backupDir, artifactRoot, dbPath, store.RestoreOptions{Force: true}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	restored, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("reopen restored db: %v", err)
	}
	defer func() { _ = restored.Close() }()
	service2, err := profile.NewService(newProfileRegistry(restored), profile.Config{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService restored: %v", err)
	}
	after := mustListFacts(t, service2)
	if len(after) != 4 {
		t.Fatalf("restored facts = %+v", after)
	}
	for _, fact := range after {
		if statuses[fact.Value] != fact.Status {
			t.Errorf("fact %q restored status = %q, want %q", fact.Value, fact.Status, statuses[fact.Value])
		}
	}
	retrievedAfter, err := service2.Retrieve(t.Context(), &profile.RetrieveQuery{OwnerID: profileLocalOwner, MaxFacts: 10, MaxTokens: 1024}, now)
	if err != nil {
		t.Fatalf("Retrieve after restore: %v", err)
	}
	if len(retrievedAfter.Facts) != 1 || retrievedAfter.Facts[0].Value != "Go" {
		t.Fatalf("retrieved after restore = %+v", retrievedAfter.Facts)
	}
	goFact, err := service2.GetFact(t.Context(), profileLocalOwner, accepted.ID)
	if err != nil {
		t.Fatalf("GetFact restored: %v", err)
	}
	goEvidence, err := service2.EvidenceFor(t.Context(), accepted.ID)
	if err != nil {
		t.Fatalf("EvidenceFor restored: %v", err)
	}
	if goFact.Confidence != goBefore.Confidence || len(goEvidence) != len(goEvidenceBefore) {
		t.Errorf("restored go fact = %+v (want confidence %v), evidence = %+v (want %d)", goFact, goBefore.Confidence, goEvidence, len(goEvidenceBefore))
	}
	if _, err := service2.ProposeFact(t.Context(), profileLocalOwner, "language", "backend", "Go", evidence("evt-bk-1", "i write backends in Go"), now); err != nil {
		t.Fatalf("re-extraction after restore must stay idempotent: %v", err)
	}
	reListed := mustListFacts(t, service2)
	if len(reListed) != 4 {
		t.Fatalf("re-extraction resurrected rows: %+v", reListed)
	}
}

func mustListFacts(t *testing.T, service *profile.Service) []profile.Fact {
	t.Helper()
	facts, err := service.ListFacts(t.Context(), profileLocalOwner, "", "", 100)
	if err != nil {
		t.Fatalf("ListFacts: %v", err)
	}
	return facts
}

func ptrTime(at time.Time) *time.Time {
	return &at
}

func retrieveProfileValues(t *testing.T, service *profile.Service, query string, now time.Time) []string {
	t.Helper()
	part, err := service.Retrieve(t.Context(), &profile.RetrieveQuery{
		OwnerID: profileLocalOwner, Query: query, MaxFacts: 10, MaxTokens: 1024,
	}, now)
	if err != nil {
		t.Fatalf("Retrieve(%q): %v", query, err)
	}
	values := make([]string, 0, len(part.Facts))
	for _, fact := range part.Facts {
		values = append(values, fact.Value)
	}
	return values
}

func TestProfileFTSRebuild(t *testing.T) {
	cfgPath := writeProfileCLIConfig(t)
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	now := time.Now().UTC()
	service, err := profile.NewService(newProfileRegistry(db), profile.Config{MinConfidence: 0.70})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	for i, value := range []string{"neovim", "zoxide", "fzf"} {
		if _, err := service.SetFact(t.Context(), profileLocalOwner, "tool", "cli-"+strconv.Itoa(i), value, nil, now); err != nil {
			t.Fatalf("SetFact %s: %v", value, err)
		}
	}
	hits := retrieveProfileValues(t, service, "neovim", now)
	if len(hits) != 1 || hits[0] != "neovim" {
		t.Fatalf("search before wipe: %v", hits)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO profile_fact_fts(profile_fact_fts) VALUES ('delete-all')`); err != nil {
		t.Fatalf("delete-all: %v", err)
	}
	hits = retrieveProfileValues(t, service, "neovim", now)
	if len(hits) != 0 {
		t.Fatalf("search after wipe: %v", hits)
	}
	if err := RebuildProfileFTS(t.Context(), db); err != nil {
		t.Fatalf("RebuildProfileFTS: %v", err)
	}
	hits = retrieveProfileValues(t, service, "neovim", now)
	if len(hits) != 1 || hits[0] != "neovim" {
		t.Fatalf("search after rebuild: %v", hits)
	}
	facts := mustListFacts(t, service)
	if len(facts) != 3 {
		t.Fatalf("rebuild must not touch lifecycle rows: %+v", facts)
	}
	for _, fact := range facts {
		if fact.Status != profile.StatusActive {
			t.Errorf("fact %q status = %q after rebuild", fact.Value, fact.Status)
		}
	}
}

func TestProfilingProducerResolves(t *testing.T) {
	cfgPath := writeProfileCLIConfig(t)
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := profilingProducer(nil); err == nil {
		t.Errorf("expected nil config rejection")
	}
	if _, err := profilingProducer(loaded.Config); err == nil {
		t.Errorf("expected missing route rejection")
	}
	routedDir := t.TempDir()
	routed := routedDir + "/routed.yaml"
	content := `version: 1
models:
  definitions:
    primary:
      protocol: openai_chat_compat
      model: test-model
      api_key_env: AURA_TEST_MODEL_KEY
      capabilities:
        streaming: true
        tools: true
        context_tokens: 200000
        tokenizer: test
  routing:
    profiling: primary
model_routes:
  primary:
    candidates: [primary]
storage:
  path: ` + routedDir + `
tools:
  workspace: ` + routedDir + `
skills:
  roots: [` + routedDir + `]
context:
  recent_complete_turns: 5
profile:
  prompt_version: v1
`
	if err := os.WriteFile(routed, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	loadedRouted, err := config.Load(routed)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	producer, err := profilingProducer(loadedRouted.Config)
	if err != nil {
		t.Fatalf("profilingProducer: %v", err)
	}
	if producer.Model != "test-model" || producer.Provider == "" {
		t.Errorf("producer = %+v", producer)
	}
}

func TestProfileModelCallerExtracts(t *testing.T) {
	llm := &fakeContextLLM{texts: []string{`[{"category":"tool","key":"editor","value":"neovim","sources":[1]}]`}}
	caller := newProfileModelCaller(llm, profile.ModelProducer{Provider: "p", Model: "m"}, 2048)
	var nilCtx context.Context
	if _, err := caller.Extract(nilCtx, &profile.ExtractRequest{}); err == nil {
		t.Errorf("expected nil context rejection")
	}
	if _, err := caller.Extract(t.Context(), nil); err == nil {
		t.Errorf("expected nil request rejection")
	}
	if _, err := newProfileModelCaller(nil, profile.ModelProducer{}, 2048).Extract(t.Context(), &profile.ExtractRequest{MaxOutputTokens: 64}); err == nil {
		t.Errorf("expected missing model rejection")
	}
	result, err := caller.Extract(t.Context(), &profile.ExtractRequest{
		Task: "profiling", Prompt: "extract", MaxOutputTokens: 512,
		Sources: []profile.ExtractSource{{Sequence: 1, EventID: "evt-1", Text: "i use neovim"}},
	})
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if result.Producer.Model != "m" || !strings.Contains(result.Text, "neovim") {
		t.Errorf("result = %+v", result)
	}
	oversized := &fakeContextLLM{texts: []string{strings.Repeat("x", 9000)}}
	if _, err := newProfileModelCaller(oversized, profile.ModelProducer{}, 64).Extract(t.Context(), &profile.ExtractRequest{MaxOutputTokens: 64}); err == nil {
		t.Errorf("expected oversized rejection")
	}
}

type profileTestScanner struct{}

func (profileTestScanner) Contains(string) bool { return false }

func TestNewProfileExtractorWiresTelemetry(t *testing.T) {
	cfgPath := writeProfileCLIConfig(t)
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	db, err := openStorage(t.Context(), loaded.Config)
	if err != nil {
		t.Fatalf("openStorage: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, _, err := newProfileExtractor(nil, db, nil, nil, nil, 64); err == nil {
		t.Errorf("expected nil config rejection")
	}
	if _, _, err := newProfileExtractor(loaded.Config, nil, nil, nil, nil, 64); err == nil {
		t.Errorf("expected nil db rejection")
	}
	if _, _, err := newProfileExtractor(loaded.Config, db, nil, nil, nil, 64); err == nil {
		t.Errorf("expected nil scanner rejection")
	}
	if _, _, err := newProfileExtractor(loaded.Config, db, &fakeContextLLM{}, nil, profileTestScanner{}, 64); err == nil {
		t.Errorf("expected missing route rejection")
	}
	routedDir := t.TempDir()
	routed := routedDir + "/routed.yaml"
	content := `version: 1
models:
  definitions:
    primary:
      protocol: openai_chat_compat
      model: test-model
      api_key_env: AURA_TEST_MODEL_KEY
      capabilities:
        streaming: true
        tools: true
        context_tokens: 200000
        tokenizer: test
  routing:
    profiling: primary
model_routes:
  primary:
    candidates: [primary]
storage:
  path: ` + routedDir + `
tools:
  workspace: ` + routedDir + `
skills:
  roots: [` + routedDir + `]
context:
  recent_complete_turns: 5
profile:
  prompt_version: v1
`
	if err := os.WriteFile(routed, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	loadedRouted, err := config.Load(routed)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	routedDB, err := openStorage(t.Context(), loadedRouted.Config)
	if err != nil {
		t.Fatalf("openStorage routed: %v", err)
	}
	defer func() { _ = routedDB.Close() }()
	reader := sdkmetric.NewManualReader()
	extractor, recorder, err := newProfileExtractor(loadedRouted.Config, routedDB, &fakeContextLLM{}, sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)), profileTestScanner{}, 512)
	if err != nil {
		t.Fatalf("newProfileExtractor: %v", err)
	}
	if extractor == nil || recorder == nil {
		t.Fatalf("extractor = %v, recorder = %v", extractor, recorder)
	}
	if err := extractor.Enqueue(profile.Job{
		OwnerID: profileLocalOwner, SessionID: "sess-wire",
		Events: []profile.JobEvent{{ID: "evt-wire-1", Sequence: 1, Text: "hello"}},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	rm := metricdata.ResourceMetrics{}
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	found := false
	for _, scope := range rm.ScopeMetrics {
		for _, point := range scope.Metrics {
			if point.Name == telemetry.MetricProfileExtractionQueueDepth {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("queue depth gauge missing from collected metrics")
	}
}

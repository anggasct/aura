package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func TestEmbeddingOptInConfigValidationAndEnvBindings(t *testing.T) {
	t.Run("memory.embedding_enabled without knowledge.embedding fails config_invalid", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("version: 1\nmemory:\n  enabled: false\n  embedding_enabled: true\n"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		_, err := appconfig.Load(path)
		if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
			t.Fatalf("Load(memory.embedding_enabled without knowledge.embedding) = %v, want %s", err, configkit.ErrorCodeInvalid)
		}
	})

	t.Run("profile.embedding_enabled without knowledge.embedding fails config_invalid", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("version: 1\nprofile:\n  enabled: false\n  embedding_enabled: true\n"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		_, err := appconfig.Load(path)
		if !fault.HasCode(err, configkit.ErrorCodeInvalid) {
			t.Fatalf("Load(profile.embedding_enabled without knowledge.embedding) = %v, want %s", err, configkit.ErrorCodeInvalid)
		}
	})

	t.Run("env bindings enable memory and profile embeddings when knowledge.embedding is configured", func(t *testing.T) {
		t.Setenv("AURA_MEMORY_EMBEDDING_ENABLED", "true")
		t.Setenv("AURA_PROFILE_EMBEDDING_ENABLED", "true")
		path := filepath.Join(t.TempDir(), "config.yaml")
		content := `version: 1
knowledge:
  embedding:
    base_url: http://127.0.0.1:11434/v1
    model: nomic-embed-text
    dimensions: 4
`
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		loaded, err := appconfig.Load(path)
		if err != nil {
			t.Fatalf("Load with env bindings: %v", err)
		}
		if !loaded.Config.Memory.EmbeddingEnabled {
			t.Error("AURA_MEMORY_EMBEDDING_ENABLED did not set Memory.EmbeddingEnabled")
		}
		if loaded.Config.Profile == nil || !loaded.Config.Profile.EmbeddingEnabled {
			t.Error("AURA_PROFILE_EMBEDDING_ENABLED did not set Profile.EmbeddingEnabled")
		}
	})
}

func TestCLIAndDaemonMemoryProfileEmbeddingWiringScreensSecretsFailClosed(t *testing.T) {
	restoreDefaultLogger(t)
	var embedCalls atomic.Int64
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		embedCalls.Add(1)
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		resp := struct {
			Data []item `json:"data"`
		}{Data: make([]item, len(req.Input))}
		for i := range req.Input {
			resp.Data[i] = item{Index: i, Embedding: []float64{1, 0, 0, 0}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(embedServer.Close)

	dataRoot := t.TempDir()
	workspaceDir := t.TempDir()
	t.Setenv("AURA_TEST_TELEMETRY_SECRET", "extra-vault-secret-777")
	cfgPath := filepath.Join(dataRoot, "config.yaml")
	cfgYAML := `version: 1
storage:
  path: ` + dataRoot + `
tools:
  workspace: ` + workspaceDir + `
telemetry:
  credential_ref: env://AURA_TEST_TELEMETRY_SECRET
knowledge:
  embedding:
    base_url: ` + embedServer.URL + `
    model: test-embed
    dimensions: 4
memory:
  enabled: true
  embedding_enabled: true
profile:
  enabled: true
  embedding_enabled: true
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	seedConfiguredStorage(t, cfgPath)

	loaded, err := appconfig.Load(cfgPath)
	if err != nil {
		t.Fatalf("appconfig.Load: %v", err)
	}
	cmdApp, err := Build(t.Context(), &Options{
		Surface:   SurfaceCommand,
		Command:   CommandOptions{Parts: []Part{PartMemory, PartProfile}},
		Config:    &loaded,
		LogLevel:  "warn",
		LogFormat: "text",
		LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("Build(SurfaceCommand): %v", err)
	}
	t.Cleanup(func() { _ = cmdApp.Close(t.Context()) })

	now := time.Now().UTC()
	sessions := turnstore.NewSessionService(cmdApp.db)
	_ = sessions.Create(t.Context(), &turn.Session{ID: "sess-cli", OwnerID: principal.OwnerID, CreatedAt: now, UpdatedAt: now, Metadata: []byte(`{}`)})

	memStore, err := memory.NewSQLiteStore(cmdApp.db, sessions)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	clean := false
	if _, err := memStore.UpsertDocument(t.Context(), &memory.StoredDocument{
		ID: "mem-cli-clean", OwnerID: principal.OwnerID, SessionID: "sess-cli", Kind: memory.KindEventText,
		FromSequence: 1, ToSequence: 1, Content: "terbang ke Jepang", TrustLabel: "owner_input", Tainted: &clean, CreatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertDocument(clean): %v", err)
	}
	if _, err := memStore.UpsertDocument(t.Context(), &memory.StoredDocument{
		ID: "mem-cli-secret", OwnerID: principal.OwnerID, SessionID: "sess-cli", Kind: memory.KindEventText,
		FromSequence: 2, ToSequence: 2, Content: "leaks extra-vault-secret-777 value", TrustLabel: "owner_input", Tainted: &clean, CreatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertDocument(secret): %v", err)
	}

	embedded, err := cmdApp.memory.BackfillEmbeddingsOnce(t.Context(), 10)
	if err != nil {
		t.Fatalf("BackfillEmbeddingsOnce: %v", err)
	}
	if embedded != 1 {
		t.Fatalf("CLI PartMemory embedded = %d, want 1 (additional configured secret blocked)", embedded)
	}
	callsBeforeSecretQuery := embedCalls.Load()
	_, mode, err := cmdApp.memory.SearchWithMode(t.Context(), &memory.SearchRequest{
		OwnerID:        principal.OwnerID,
		PrincipalClass: principal.ClassOwner,
		Query:          "find extra-vault-secret-777 now",
		Now:            now,
		Limit:          5,
	})
	if err != nil {
		t.Fatalf("SearchWithMode(secret query): %v", err)
	}
	if mode != memory.ModeLexical || embedCalls.Load() != callsBeforeSecretQuery {
		t.Fatalf("CLI PartMemory sent additional configured secret query to embedder: mode=%q calls=%d->%d", mode, callsBeforeSecretQuery, embedCalls.Load())
	}

	if _, err := cmdApp.profile.SetFact(t.Context(), principal.OwnerID, "preference", "secret_check", "contains extra-vault-secret-777", nil, now); !fault.HasCode(err, profile.ErrorCodeProfileInvalid) {
		t.Fatalf("CLI PartProfile SetFact with additional secret = %v, want %s", err, profile.ErrorCodeProfileInvalid)
	}
	if _, err := cmdApp.profile.SetFact(t.Context(), principal.OwnerID, "preference", "destination", "terbang ke Jepang", nil, now); err != nil {
		t.Fatalf("CLI PartProfile SetFact(clean): %v", err)
	}
	part, err := cmdApp.profile.Retrieve(t.Context(), &profile.RetrieveQuery{
		OwnerID:      principal.OwnerID,
		Query:        "pesawat ke Tokyo",
		MaxFacts:     5,
		MaxTokens:    512,
		VerifiedOnly: true,
	}, now)
	if err != nil {
		t.Fatalf("CLI PartProfile Retrieve: %v", err)
	}
	if len(part.Facts) != 1 || part.Facts[0].Value != "terbang ke Jepang" {
		t.Fatalf("CLI PartProfile hybrid Retrieve = %+v, want semantic match", part.Facts)
	}

	t.Setenv("AURA_TEST_TELEMETRY_SECRET", "")
	callsBeforeUnresolved := embedCalls.Load()
	_, mode, err = cmdApp.memory.SearchWithMode(t.Context(), &memory.SearchRequest{
		OwnerID:        principal.OwnerID,
		PrincipalClass: principal.ClassOwner,
		Query:          "pesawat ke Tokyo",
		Now:            now,
		Limit:          5,
	})
	if err != nil {
		t.Fatalf("SearchWithMode(unresolved): %v", err)
	}
	if mode != memory.ModeLexical || embedCalls.Load() != callsBeforeUnresolved {
		t.Fatalf("unresolved secret ref did not fail closed: mode=%q calls=%d->%d", mode, callsBeforeUnresolved, embedCalls.Load())
	}
}

func TestDaemonEmbeddingBackgroundRegistrationAndStrictSecrets(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the server workspace adapter needs Linux: the filesystem adapters refuse other systems")
	}
	restoreDefaultLogger(t)
	t.Setenv("AURA_TEST_MODEL_KEY", "test-key")
	t.Setenv("AURA_TEST_TELEMETRY_SECRET", "daemon-vault-secret-888")
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0,0]}]}`))
	}))
	t.Cleanup(embedServer.Close)

	dataRoot := t.TempDir()
	workspaceDir := t.TempDir()
	cfgPath := filepath.Join(dataRoot, "config.yaml")
	cfgYAML := `version: 1
storage:
  path: ` + dataRoot + `
tools:
  workspace: ` + workspaceDir + `
health:
  listen: 127.0.0.1:18999
telemetry:
  credential_ref: env://AURA_TEST_TELEMETRY_SECRET
` + compositionModelsYAML + `knowledge:
  embedding:
    base_url: ` + embedServer.URL + `
    model: test-embed
    dimensions: 4
memory:
  enabled: true
  embedding_enabled: true
profile:
  enabled: true
  embedding_enabled: true
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	seedConfiguredStorage(t, cfgPath)
	loaded, err := appconfig.Load(cfgPath)
	if err != nil {
		t.Fatalf("appconfig.Load: %v", err)
	}
	built, err := Build(t.Context(), &Options{
		Surface:   SurfaceServer,
		Config:    &loaded,
		LogLevel:  "warn",
		LogFormat: "text",
		LogOutput: io.Discard,
	})
	if err != nil {
		t.Fatalf("Build(SurfaceServer): %v", err)
	}
	t.Cleanup(func() { _ = built.Close(t.Context()) })

	if !slices.Contains(built.group.Names(), "memory_embedding") {
		t.Fatalf("server lifecycle entries = %v, want memory_embedding registered", built.group.Names())
	}
	if !strings.Contains(strings.Join(built.group.Names(), ","), "profile_extraction") {
		t.Fatalf("server lifecycle entries = %v, want profile_extraction registered", built.group.Names())
	}
}

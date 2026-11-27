package knowledge

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
)

func TestModuleNewValidatesDependencies(t *testing.T) {
	if _, err := New(t.Context(), nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("nil deps err = %v, want %q", err, ErrorCodeInvalidArgument)
	}
	if _, err := New(t.Context(), &ModuleDeps{}); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("empty db path err = %v, want %q", err, ErrorCodeInvalidArgument)
	}
	if _, err := New(t.Context(), &ModuleDeps{
		DBPath: filepath.Join(t.TempDir(), "aura.db"),
		Scopes: []PersonaScope{{Persona: "lobby", Class: "owner", Collections: []string{"faq"}}},
	}); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("nil secrets with scopes err = %v, want %q", err, ErrorCodeInvalidArgument)
	}
}

func TestModuleNewAndCloseAreIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "aura.db")
	db, err := sqlite.Open(t.Context(), dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	mod, err := New(t.Context(), &ModuleDeps{
		DBPath:  dbPath,
		Scopes:  []PersonaScope{{Persona: "lobby", Class: "owner", Collections: []string{"faq"}}},
		Secrets: &fakeSecrets{},
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(mod.Tools()) != 1 || mod.Retriever() == nil || mod.VectorLoader() != nil {
		t.Fatalf("tools=%d retriever=%v loader=%v", len(mod.Tools()), mod.Retriever(), mod.VectorLoader() != nil)
	}
	if len(mod.HealthChecks()) != 2 {
		t.Fatalf("health checks = %d, want 2", len(mod.HealthChecks()))
	}
	for range 2 {
		if err := mod.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

func TestModuleExposesVectorLoaderWithoutSpawningGoroutine(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "aura.db")
	db, err := sqlite.Open(t.Context(), dbPath, sqlite.Options{})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	mod, err := New(t.Context(), &ModuleDeps{
		Config: &Config{Embedding: &EmbeddingConfig{
			BaseURL:         "https://api.example.com/v1",
			Model:           "embed-v1",
			Dimensions:      4,
			DimensionsField: DefaultEmbeddingDimensionsField,
			RoleStyle:       EmbeddingRoleNone,
			BatchSize:       DefaultEmbeddingBatchSize,
			MaxMemory:       MinEmbeddingMaxMemory,
		}},
		DBPath:  dbPath,
		Scopes:  []PersonaScope{{Persona: "lobby", Class: "owner", Collections: []string{"faq"}}},
		Secrets: &fakeSecrets{},
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if mod.VectorLoader() == nil || mod.VectorIndex() == nil {
		t.Fatal("VectorLoader/VectorIndex nil with configured embedding")
	}
	if err := mod.Close(); err != nil {
		t.Fatalf("Close without running VectorLoader: %v", err)
	}
}

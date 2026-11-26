package knowledge

import (
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/embedding"
	"github.com/anggasct/aura/internal/platform/secret/secrettest"
)

func validEmbeddingConfig() *EmbeddingConfig {
	return &EmbeddingConfig{
		BaseURL:         "https://api.example.com/v1",
		Model:           "text-embedding-3-small",
		Dimensions:      1536,
		DimensionsField: DefaultEmbeddingDimensionsField,
		RoleStyle:       EmbeddingRoleNone,
		BatchSize:       DefaultEmbeddingBatchSize,
		MaxMemory:       DefaultEmbeddingMaxMemory,
	}
}

func TestKnowledgeConfigValidate(t *testing.T) {
	if err := (*Config)(nil).Validate(); err != nil {
		t.Fatalf("nil Config.Validate(): %v", err)
	}
	if err := (&Config{}).Validate(); err != nil {
		t.Fatalf("empty Config.Validate(): %v", err)
	}
	if d := Default(); d.Embedding != nil {
		t.Fatalf("Default().Embedding = %+v, want nil", d.Embedding)
	}

	validCases := map[string]func(*EmbeddingConfig){
		"defaults":           func(*EmbeddingConfig) {},
		"dimensions min":     func(e *EmbeddingConfig) { e.Dimensions = 1 },
		"dimensions max":     func(e *EmbeddingConfig) { e.Dimensions = MaxEmbeddingDimensions },
		"batch size min":     func(e *EmbeddingConfig) { e.BatchSize = 1 },
		"batch size max":     func(e *EmbeddingConfig) { e.BatchSize = MaxEmbeddingBatchSize },
		"max memory min":     func(e *EmbeddingConfig) { e.MaxMemory = MinEmbeddingMaxMemory },
		"role input_type":    func(e *EmbeddingConfig) { e.RoleStyle = EmbeddingRoleInputType },
		"role prefix query":  func(e *EmbeddingConfig) { e.RoleStyle = EmbeddingRolePrefix; e.QueryPrefix = "Q: " },
		"role prefix doc":    func(e *EmbeddingConfig) { e.RoleStyle = EmbeddingRolePrefix; e.DocumentPrefix = "D: " },
		"loopback ipv4 http": func(e *EmbeddingConfig) { e.BaseURL = "http://127.0.0.1:11434/v1" },
		"loopback ipv6 http": func(e *EmbeddingConfig) { e.BaseURL = "http://[::1]:11434/v1" },
	}
	for name, mutate := range validCases {
		t.Run("valid "+name, func(t *testing.T) {
			emb := validEmbeddingConfig()
			mutate(emb)
			if err := (&Config{Embedding: emb}).Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}

	invalidCases := map[string]struct {
		mutate func(*EmbeddingConfig)
		detail string
	}{
		"dimensions zero":        {mutate: func(e *EmbeddingConfig) { e.Dimensions = 0 }, detail: "embedding.dimensions"},
		"dimensions above max":   {mutate: func(e *EmbeddingConfig) { e.Dimensions = MaxEmbeddingDimensions + 1 }, detail: "embedding.dimensions"},
		"batch size zero":        {mutate: func(e *EmbeddingConfig) { e.BatchSize = 0 }, detail: "embedding.batch_size"},
		"batch size above max":   {mutate: func(e *EmbeddingConfig) { e.BatchSize = MaxEmbeddingBatchSize + 1 }, detail: "embedding.batch_size"},
		"max memory below min":   {mutate: func(e *EmbeddingConfig) { e.MaxMemory = MinEmbeddingMaxMemory - 1 }, detail: "embedding.max_memory"},
		"unknown role style":     {mutate: func(e *EmbeddingConfig) { e.RoleStyle = "task" }, detail: "embedding.role_style"},
		"prefix without style":   {mutate: func(e *EmbeddingConfig) { e.QueryPrefix = "q: " }, detail: "prefixes require role_style prefix"},
		"prefix style no text":   {mutate: func(e *EmbeddingConfig) { e.RoleStyle = EmbeddingRolePrefix }, detail: "prefix requires query_prefix or document_prefix"},
		"control char in prefix": {mutate: func(e *EmbeddingConfig) { e.RoleStyle = EmbeddingRolePrefix; e.QueryPrefix = "a\nb" }, detail: "embedding.query_prefix"},
		"prefix too long": {mutate: func(e *EmbeddingConfig) {
			e.RoleStyle = EmbeddingRolePrefix
			e.DocumentPrefix = strings.Repeat("a", maxEmbeddingPrefixLength+1)
		}, detail: "embedding.document_prefix"},
		"reserved field model": {mutate: func(e *EmbeddingConfig) { e.DimensionsField = "model" }, detail: "must not be a reserved request field"},
		"reserved field input": {mutate: func(e *EmbeddingConfig) { e.DimensionsField = "input" }, detail: "must not be a reserved request field"},
		"invalid field name":   {mutate: func(e *EmbeddingConfig) { e.DimensionsField = "Dim-Size" }, detail: "must be a lowercase request field name"},
		"empty model":          {mutate: func(e *EmbeddingConfig) { e.Model = "" }, detail: "embedding.model"},
		"model with space":     {mutate: func(e *EmbeddingConfig) { e.Model = "a b" }, detail: "embedding.model"},
		"loopback localhost":   {mutate: func(e *EmbeddingConfig) { e.BaseURL = "http://localhost:11434/v1" }, detail: "embedding.base_url"},
		"loopback 127.0.0.2":   {mutate: func(e *EmbeddingConfig) { e.BaseURL = "http://127.0.0.2/v1" }, detail: "embedding.base_url"},
	}
	for name, tc := range invalidCases {
		t.Run("invalid "+name, func(t *testing.T) {
			emb := validEmbeddingConfig()
			tc.mutate(emb)
			err := (&Config{Embedding: emb}).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("Validate() = %v, want %q", err, tc.detail)
			}
		})
	}
}

func TestEmbeddingConfigEndpointAndClientConfig(t *testing.T) {
	if _, err := (*EmbeddingConfig)(nil).Endpoint(); err == nil {
		t.Fatal("nil Endpoint() succeeded")
	}
	if cfg := (*EmbeddingConfig)(nil).ClientConfig(); cfg != (embedding.Config{}) {
		t.Fatalf("nil ClientConfig() = %+v, want zero", cfg)
	}
	base := validEmbeddingConfig()
	baseClient := base.ClientConfig()
	reference, err := embedding.Fingerprint(&baseClient)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	same := map[string]func(*EmbeddingConfig){
		"batch size":     func(e *EmbeddingConfig) { e.BatchSize = 7 },
		"credential":     func(e *EmbeddingConfig) { e.CredentialRef = secrettest.Ref("env://OTHER") },
		"memory":         func(e *EmbeddingConfig) { e.MaxMemory = configkit.ByteSize(1 << 30) },
		"public queries": func(e *EmbeddingConfig) { e.PublicQueries = true },
	}
	for name, mutate := range same {
		t.Run("fingerprint ignores "+name, func(t *testing.T) {
			emb := validEmbeddingConfig()
			mutate(emb)
			clientCfg := emb.ClientConfig()
			got, err := embedding.Fingerprint(&clientCfg)
			if err != nil || got != reference {
				t.Fatalf("Fingerprint after %s = %q, %v; want %q", name, got, err, reference)
			}
		})
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeMemoryConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoad_MemoryDefaults(t *testing.T) {
	path := writeMemoryConfig(t, "version: 1\n")
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	memory := res.Config.Memory
	if !memory.Enabled {
		t.Error("memory must be enabled by default")
	}
	if memory.MaxDocuments != 10 {
		t.Errorf("max_documents = %d, want 10", memory.MaxDocuments)
	}
	if memory.RecallTokenBudget != 2000 {
		t.Errorf("recall_token_budget = %d, want 2000", memory.RecallTokenBudget)
	}
	if memory.SummaryPromptVersion != "memory-summary-v1" {
		t.Errorf("summary_prompt_version = %q", memory.SummaryPromptVersion)
	}
	if memory.SummaryTTL != Duration(720*time.Hour) {
		t.Errorf("summary_ttl = %v", memory.SummaryTTL)
	}
}

func TestLoad_MemoryValid(t *testing.T) {
	path := writeMemoryConfig(t, `version: 1
memory:
  enabled: false
  max_documents: 25
  recall_token_budget: 4000
  summary_prompt_version: memory-summary-v2
  summary_ttl: 48h
`)
	res, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	memory := res.Config.Memory
	if memory.Enabled || memory.MaxDocuments != 25 || memory.EnglishStemming {
		t.Errorf("memory = %+v", memory)
	}
	if strings.TrimSpace(memory.Locale) != "" {
		t.Errorf("locale = %q, want empty", memory.Locale)
	}
}

func TestLoad_MemoryRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"zero documents", "  max_documents: 0\n"},
		{"over hard max documents", "  max_documents: 51\n"},
		{"zero budget", "  recall_token_budget: 0\n"},
		{"over hard max budget", "  recall_token_budget: 8001\n"},
		{"empty prompt version", "  summary_prompt_version: \"\"\n"},
		{"zero ttl", "  summary_ttl: 0s\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeMemoryConfig(t, "version: 1\nmemory:\n"+tc.body)
			if _, err := Load(path); err == nil {
				t.Errorf("invalid memory accepted:\n%s", tc.body)
			}
		})
	}
}

func TestLoad_MemoryRejectsUnsupportedTokenizerOptions(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"stemming opt-in", "  english_stemming: true\n"},
		{"explicit locale", "  locale: en\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeMemoryConfig(t, "version: 1\nmemory:\n"+tc.body)
			if _, err := Load(path); err == nil {
				t.Errorf("unsupported tokenizer option accepted:\n%s", tc.body)
			}
		})
	}
}

func TestLoad_MemoryRejectsBadShapes(t *testing.T) {
	path := writeMemoryConfig(t, "version: 1\nmemory:\n  max_documents: ten\n")
	if _, err := Load(path); err == nil {
		t.Error("non-integer max_documents accepted")
	}
	path = writeMemoryConfig(t, "version: 1\nmemory:\n  bogus_key: 1\n")
	if _, err := Load(path); err == nil {
		t.Error("unknown memory key accepted")
	}
}

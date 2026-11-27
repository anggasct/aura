package agent

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"testing"
)

type logCaptureHandler struct {
	records []slog.Record
}

func (h *logCaptureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *logCaptureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *logCaptureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }

func (h *logCaptureHandler) WithGroup(_ string) slog.Handler { return h }

func TestBundlesHaveTheSameKeysAndPlaceholders(t *testing.T) {
	enBundle, ok := messageBundles[DefaultMessageLanguage]
	if !ok {
		t.Fatalf("missing default bundle %q", DefaultMessageLanguage)
	}
	enKeys := slices.Sorted(maps.Keys(enBundle))
	if len(enKeys) == 0 {
		t.Fatal("default bundle is empty")
	}
	for key := range messagePlaceholders {
		if _, exists := enBundle[key]; !exists {
			t.Errorf("placeholder table declares unknown key %q", key)
		}
	}
	for _, lang := range slices.Sorted(maps.Keys(messageBundles)) {
		bundle := messageBundles[lang]
		gotKeys := slices.Sorted(maps.Keys(bundle))
		if !slices.Equal(gotKeys, enKeys) {
			t.Errorf("bundle %q keys = %v, want %v", lang, gotKeys, enKeys)
		}
		for _, key := range gotKeys {
			if err := validateMessageValue(key, bundle[key]); err != nil {
				t.Errorf("bundle %q key %q invalid: %v", lang, key, err)
			}
		}
	}
}

func TestMessageResolution(t *testing.T) {
	cases := []struct {
		name         string
		language     string
		override     map[string]string
		key          string
		want         string
		wantFallback int
		wantLogLang  string
	}{
		{
			name:     "persona override wins over id bundle",
			language: "id",
			override: map[string]string{MessageStatusWorking: "Sebentar ya…"},
			key:      MessageStatusWorking,
			want:     "Sebentar ya…",
		},
		{
			name:     "id bundle wins over en default",
			language: "id",
			key:      MessageStatusWorking,
			want:     "Sedang mengerjakan…",
		},
		{
			name:     "id-ID selects id bundle without log",
			language: "id-ID",
			key:      MessageStatusStillWorking,
			want:     "Masih mengerjakan…",
		},
		{
			name:     "en selects en bundle without log",
			language: "en",
			key:      MessageStatusWorking,
			want:     "Working…",
		},
		{
			name:         "ms falls back to en with one boot log",
			language:     "ms",
			key:          MessageStatusWorking,
			want:         "Working…",
			wantFallback: 1,
			wantLogLang:  "ms",
		},
		{
			name:         "und falls back to en with one boot log",
			language:     "und",
			key:          MessageStatusWorking,
			want:         "Working…",
			wantFallback: 1,
			wantLogLang:  "und",
		},
		{
			name:         "fr falls back to en with one boot log",
			language:     "fr",
			key:          MessageStatusWorking,
			want:         "Working…",
			wantFallback: 1,
			wantLogLang:  "fr",
		},
		{
			name:         "id-Arab falls back to en with one boot log",
			language:     "id-Arab",
			key:          MessageStatusWorking,
			want:         "Working…",
			wantFallback: 1,
			wantLogLang:  "id-Arab",
		},
		{
			name:         "en-Shaw falls back to en with one boot log",
			language:     "en-Shaw",
			key:          MessageStatusWorking,
			want:         "Working…",
			wantFallback: 1,
			wantLogLang:  "en-Shaw",
		},
		{
			name:     "no language uses en without log",
			language: "",
			key:      MessageStatusWorking,
			want:     "Working…",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			override := kantorOverride()
			override.Messages = tc.override
			registry, err := Build([]Persona{override}, validTools, validToolCatalog, []string{"primary"}, tc.language)
			if err != nil {
				t.Fatalf("Build(%q): %v", tc.language, err)
			}
			kantor, ok := registry.Lookup("kantor")
			if !ok {
				t.Fatal("kantor persona not found")
			}
			if got := kantor.Message(tc.key); got != tc.want {
				t.Fatalf("Message(%q) = %q, want %q", tc.key, got, tc.want)
			}

			handler := &logCaptureHandler{}
			LogPersonas(t.Context(), slog.New(handler), registry)
			fallbackLogs := 0
			gotLang := ""
			for _, rec := range handler.records {
				if rec.Message == "persona messages use en" {
					fallbackLogs++
					rec.Attrs(func(a slog.Attr) bool {
						if a.Key == "language" {
							gotLang = a.Value.String()
						}
						return true
					})
				}
			}
			if fallbackLogs != tc.wantFallback {
				t.Fatalf("fallback log count = %d, want %d", fallbackLogs, tc.wantFallback)
			}
			if tc.wantFallback > 0 && gotLang != tc.wantLogLang {
				t.Fatalf("fallback log language = %q, want %q", gotLang, tc.wantLogLang)
			}
		})
	}
}

func TestFormatMessageReplacesInOnePass(t *testing.T) {
	def := Definition{
		Messages: map[string]string{
			MessageWorkflowSucceeded: "Done: {goal} ({code})",
		},
	}
	got := def.FormatMessage(MessageWorkflowSucceeded, "{goal}", "{code}", "{code}", "1234")
	if got != "Done: {code} (1234)" {
		t.Fatalf("FormatMessage = %q, want one-pass substitution", got)
	}
}

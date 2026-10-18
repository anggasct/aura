package cli

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/runtime/adk"
)

type captureHandler struct {
	records []slog.Record
	level   slog.Level
}

func (h *captureHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.level
}

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r)
	return nil
}

func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

func newTestLogger(h slog.Handler) *slog.Logger {
	return slog.New(h)
}

func applyOption(t *testing.T, opt runtimeadk.ExecutorOption) string {
	t.Helper()
	e := &runtimeadk.ADKExecutor{}
	if err := opt(e); err != nil {
		t.Fatalf("apply option: %v", err)
	}
	v := reflect.ValueOf(e).Elem().FieldByName("ownerContext")
	return v.String()
}

func TestOwnerContextOptionNilOwner(t *testing.T) {
	h := &captureHandler{level: slog.LevelDebug}
	logger := newTestLogger(h)
	opt := ownerContextOption(context.Background(), nil, logger)
	got := applyOption(t, opt)
	if got != "" {
		t.Fatalf("ownerContext = %q, want empty", got)
	}
	if len(h.records) != 0 {
		t.Fatalf("expected no log records, got %d", len(h.records))
	}
}

func TestOwnerContextOptionEmptyBlock(t *testing.T) {
	h := &captureHandler{level: slog.LevelDebug}
	logger := newTestLogger(h)
	owner := &config.Owner{}
	opt := ownerContextOption(context.Background(), owner, logger)
	got := applyOption(t, opt)
	if got != "" {
		t.Fatalf("ownerContext = %q, want empty", got)
	}
	if len(h.records) != 0 {
		t.Fatalf("expected no log records, got %d", len(h.records))
	}
}

func TestOwnerContextOptionPopulatedOwner(t *testing.T) {
	h := &captureHandler{level: slog.LevelDebug}
	logger := newTestLogger(h)
	owner := &config.Owner{
		Name:        "Angga",
		Language:    "Indonesian",
		Timezone:    "Asia/Jakarta",
		Preferences: map[string]string{"style": "concise"},
	}
	opt := ownerContextOption(context.Background(), owner, logger)
	got := applyOption(t, opt)

	block := runtimeadk.RenderOwnerContext(&runtimeadk.OwnerContext{
		Name:        owner.Name,
		Language:    owner.Language,
		Timezone:    owner.Timezone,
		Preferences: owner.Preferences,
	})
	if got != block {
		t.Fatalf("ownerContext mismatch:\ngot  %q\nwant %q", got, block)
	}

	if len(h.records) != 1 {
		t.Fatalf("expected 1 log record, got %d", len(h.records))
	}
	digest := runtimeadk.OwnerContextDigest(block)
	found := false
	h.records[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "digest" && a.Value.String() == digest {
			found = true
		}
		return true
	})
	if !found {
		t.Fatalf("digest %q not found in log attrs", digest)
	}
}

func TestOwnerContextOptionOversizedBlock(t *testing.T) {
	h := &captureHandler{level: slog.LevelDebug}
	logger := newTestLogger(h)
	big := strings.Repeat("x", ownerContextWarnBytes+100)
	owner := &config.Owner{
		Name:        "Angga",
		Preferences: map[string]string{"bio": big},
	}
	opt := ownerContextOption(context.Background(), owner, logger)
	got := applyOption(t, opt)

	if got == "" {
		t.Fatal("ownerContext is empty, want non-empty")
	}
	if len(got) <= ownerContextWarnBytes {
		t.Fatalf("ownerContext length %d, want > %d", len(got), ownerContextWarnBytes)
	}

	if len(h.records) != 2 {
		t.Fatalf("expected 2 log records (info + warn), got %d", len(h.records))
	}
	if h.records[1].Level != slog.LevelWarn {
		t.Fatalf("second record level = %v, want Warn", h.records[1].Level)
	}
}

package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/fault"
)

func executeCommands(t *testing.T, open Opener, args ...string) (string, error) {
	t.Helper()
	cmds := Commands(open)
	if len(cmds) != 1 {
		t.Fatalf("Commands returned %d root commands, want 1", len(cmds))
	}
	cmd := cmds[0]
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errBuf)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestCommandsRefusesNilOpener(t *testing.T) {
	cases := [][]string{
		{"list"},
		{"check"},
		{"show", "support"},
		{"add", "support", "doc.md"},
		{"remove", "support", "doc.md"},
		{"refresh", "support"},
		{"embed", "support"},
	}
	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			_, err := executeCommands(t, nil, args...)
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("args %v err = %v, want %q", args, err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func newTestCommandService(t *testing.T, deps *Deps) *Service {
	t.Helper()
	if deps.Store == nil {
		deps.Store = &fakeStore{}
	}
	if deps.Secrets == nil {
		deps.Secrets = &fakeSecrets{}
	}
	if deps.Reducer == nil {
		deps.Reducer = func(_ context.Context, source []byte, _ int64) (string, bool, error) {
			return string(source), false, nil
		}
	}
	if deps.HTTP == nil {
		deps.HTTP = &fakeDoer{}
	}
	if deps.Actions == nil {
		deps.Actions = &recordedActions{}
	}
	svc, err := NewService(deps)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestCommandsListJSONFormatting(t *testing.T) {
	svc := newTestCommandService(t, &Deps{})
	open := func(cmd *cobra.Command, fn func(context.Context, *slog.Logger, *Service) error) error {
		return fn(cmd.Context(), slog.New(slog.DiscardHandler), svc)
	}
	out, err := executeCommands(t, open, "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var got ListOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if len(got.Collections) != 1 || got.Collections[0] != (ListRow{ID: "support", Sources: 1, Chunks: 2, Generation: 0}) {
		t.Fatalf("decoded = %+v", got)
	}
}

func TestCommandsCheckReturnsIndexDamagedAfterPrintingReport(t *testing.T) {
	store := &fakeStore{
		report: IndexReport{
			Collections:   1,
			Sources:       1,
			Chunks:        4,
			IndexedChunks: 2,
			Problems:      []string{"2 chunks missing from index"},
		},
	}
	svc := newTestCommandService(t, &Deps{Store: store})
	open := func(cmd *cobra.Command, fn func(context.Context, *slog.Logger, *Service) error) error {
		return fn(cmd.Context(), slog.New(slog.DiscardHandler), svc)
	}
	out, err := executeCommands(t, open, "check")
	if !fault.HasCode(err, ErrorCodeIndexDamaged) {
		t.Fatalf("check err = %v, want %q", err, ErrorCodeIndexDamaged)
	}
	if !strings.Contains(out, "collections=1 sources=1 chunks=4 indexed=2") || !strings.Contains(out, "problem: 2 chunks missing from index") {
		t.Fatalf("check output = %q", out)
	}
}

func TestCommandsEmbedPreviewApplyAndOffFormatting(t *testing.T) {
	vecStore := newFakeVectorStore("Thirty days.")
	embedder := &fakeEmbedder{reply: func(texts []string) [][]float32 {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0, 0}
		}
		return out
	}}
	svc := newTestCommandService(t, &Deps{
		Vectors:  vecStore,
		Embedder: embedder,
		Embedding: &EmbeddingSettings{
			Fingerprint: "fp-1",
			Destination: "api.example.com",
			Model:       "embed-v1",
			Dimensions:  4,
			BatchSize:   10,
			MaxMemory:   1 << 20,
		},
	})
	open := func(cmd *cobra.Command, fn func(context.Context, *slog.Logger, *Service) error) error {
		return fn(cmd.Context(), slog.New(slog.DiscardHandler), svc)
	}

	preview, err := executeCommands(t, open, "embed", "support")
	if err != nil {
		t.Fatalf("embed preview: %v", err)
	}
	if !strings.Contains(preview, "dry-run: collection support would send 1 of 1 chunks") || !strings.Contains(preview, "destination: api.example.com (text leaves this host)") {
		t.Fatalf("preview = %q", preview)
	}

	applied, err := executeCommands(t, open, "embed", "support", "--apply")
	if err != nil {
		t.Fatalf("embed --apply: %v", err)
	}
	if !strings.Contains(applied, "embedded: 1 chunks in 1 batches sent to api.example.com") || !strings.Contains(applied, "complete: every chunk has a vector") {
		t.Fatalf("applied = %q", applied)
	}

	offPreview, err := executeCommands(t, open, "embed", "support", "--off")
	if err != nil {
		t.Fatalf("embed --off: %v", err)
	}
	if !strings.Contains(offPreview, "dry-run: would delete 1 vectors of collection support") {
		t.Fatalf("off preview = %q", offPreview)
	}

	offApply, err := executeCommands(t, open, "embed", "support", "--off", "--apply")
	if err != nil {
		t.Fatalf("embed --off --apply: %v", err)
	}
	if !strings.Contains(offApply, "embedding off: 1 vectors deleted and the opt-in cleared") {
		t.Fatalf("off apply = %q", offApply)
	}
}

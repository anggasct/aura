package workspaceops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/workspace"
)

const (
	noRecordsLine    = "no workspace taint records"
	shortDigestChars = 12
)

type Operations struct {
	store *workspace.TaintStore
}

func NewOperations(store *workspace.TaintStore) *Operations {
	return &Operations{store: store}
}

func (o *Operations) Bindings() []operation.Binding {
	return []operation.Binding{
		{
			ID:      operationUntaint,
			Handle:  o.untaint,
			Preview: o.previewUntaint,
		},
	}
}

type untaintInput struct {
	Path string `json:"path"`
}

func parsePathInput(call *operation.Call) (string, error) {
	if call == nil {
		return "", operation.NewError(operation.ErrorCodeInvalidArgument, "call must not be nil")
	}
	var in untaintInput
	if err := call.Decode(&in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Path) == "" {
		return "", nil
	}
	if err := workspace.ValidateRelative(in.Path); err != nil {
		return "", operation.NewError(operation.ErrorCodeInputInvalid, err.Error())
	}
	return filepath.ToSlash(filepath.Clean(in.Path)), nil
}

func shortDigest(digest string) string {
	if len(digest) <= shortDigestChars {
		return digest
	}
	return digest[:shortDigestChars]
}

func recordsDigest(records []workspace.TaintRecord) string {
	parts := make([]string, 0, len(records))
	for i := range records {
		r := &records[i]
		parts = append(parts, strings.Join([]string{r.ID, r.Scope, r.Path, r.ContentSHA256, r.Source}, "|"))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func previewLines(records []workspace.TaintRecord) []string {
	lines := make([]string, 0, len(records))
	for i := range records {
		r := &records[i]
		date := r.RecordedAt.UTC().Format("2006-01-02")
		if r.Scope == workspace.ScopeWorkspace {
			lines = append(lines, fmt.Sprintf("workspace-wide  source=%s  recorded=%s", r.Source, date))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s  sha256=%s  source=%s  recorded=%s", r.Path, shortDigest(r.ContentSHA256), r.Source, date))
	}
	return lines
}

func (o *Operations) previewUntaint(ctx context.Context, call *operation.Call) (operation.Preview, error) {
	if o == nil || o.store == nil {
		return operation.Preview{}, operation.NewError(operation.ErrorCodeFailed, "workspace taint store is unavailable")
	}
	path, err := parsePathInput(call)
	if err != nil {
		return operation.Preview{}, err
	}
	records, err := o.store.List(ctx, path)
	if err != nil {
		return operation.Preview{}, operation.NewError(operation.ErrorCodeFailed, err.Error())
	}
	if len(records) == 0 {
		return operation.Preview{Lines: []string{noRecordsLine}, Digest: recordsDigest(nil), Empty: true}, nil
	}
	return operation.Preview{
		Lines:  previewLines(records),
		Digest: recordsDigest(records),
	}, nil
}

func (o *Operations) untaint(ctx context.Context, call *operation.Call) (operation.Result, error) {
	if o == nil || o.store == nil {
		return operation.Result{}, operation.NewError(operation.ErrorCodeFailed, "workspace taint store is unavailable")
	}
	path, err := parsePathInput(call)
	if err != nil {
		return operation.Result{}, err
	}
	return call.Commit(ctx, func(ctx context.Context) (operation.Result, error) {
		records, err := o.store.List(ctx, path)
		if err != nil {
			return operation.Result{}, operation.NewError(operation.ErrorCodeFailed, err.Error())
		}
		if call.PreviewDigest == "" || recordsDigest(records) != call.PreviewDigest {
			return operation.Result{}, operation.NotPerformed(
				operation.ErrorCodePreviewStale,
				operation.NewError(operation.ErrorCodePreviewStale, "workspace taint records changed since the preview"),
			)
		}
		ids := make([]string, len(records))
		for i := range records {
			ids[i] = records[i].ID
		}
		deleted, err := o.store.DeleteIDs(ctx, ids)
		if err != nil {
			return operation.Result{}, operation.NewError(operation.ErrorCodeFailed, err.Error())
		}
		return operation.JSONResult(map[string]any{
			"text": fmt.Sprintf("removed %d workspace taint record(s)", deleted),
		})
	})
}

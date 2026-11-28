package operation

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/workspace"
)

const (
	artifactPrefix   = "artifact:"
	maxArtifactIDLen = 128
	maxFileNameRunes = 255
)

type ArtifactOpener interface {
	Open(ctx context.Context, refID string) (io.ReadCloser, storage.ArtifactMetadata, error)
}

type FileDeps struct {
	Workspace string
	Artifacts ArtifactOpener
}

func (d *FileDeps) validate() error {
	if d == nil {
		return nil
	}
	if d.Workspace != "" && !filepath.IsAbs(d.Workspace) {
		return NewError(ErrorCodeInvalidArgument, "operation: the workspace must be an absolute path")
	}
	return nil
}

type openedFile struct {
	*os.File
	name      string
	mediaType string
	size      int64
}

func (f *openedFile) Name() string { return f.name }

func (f *openedFile) MediaType() string { return f.mediaType }

func (f *openedFile) Size() int64 { return f.size }

type surfaceFiles struct {
	deps      *FileDeps
	ownerOnly bool
	sessionID string
}

func (r *Registry) filesFor(caller *Caller) FileAuthority {
	return &surfaceFiles{deps: r.files, ownerOnly: caller.Surface == SurfaceAgent, sessionID: caller.SessionID}
}

func (f *surfaceFiles) Open(ctx context.Context, source string) (File, error) {
	switch {
	case strings.Contains(source, "://"):
		return nil, NewError(ErrorCodeInputInvalid, "a URL is not a file source")
	case strings.HasPrefix(source, artifactPrefix):
		return f.artifact(ctx, strings.TrimPrefix(source, artifactPrefix))
	}
	return f.workspaceFile(source)
}

func (f *surfaceFiles) workspaceFile(relative string) (File, error) {
	if f.deps == nil || f.deps.Workspace == "" {
		return nil, NewError(ErrorCodePolicyDenied, "operation: no workspace is configured for file sources")
	}
	file, err := workspace.OpenRead(f.deps.Workspace, relative)
	switch {
	case fault.HasCode(err, workspace.ErrorCodeOutside):
		return nil, wrapError(ErrorCodePolicyDenied, "the path is outside the workspace", err)
	case errors.Is(err, fs.ErrNotExist):
		return nil, wrapError(ErrorCodeSourceUnavailable, "the file does not exist", err)
	case err != nil:
		return nil, wrapError(ErrorCodeSourceUnavailable, "the file cannot be opened", err)
	}
	return regularFile(file, path.Base(filepath.ToSlash(relative)), "")
}

func (f *surfaceFiles) artifact(ctx context.Context, id string) (File, error) {
	if f.deps == nil || f.deps.Artifacts == nil || id == "" || len(id) > maxArtifactIDLen {
		return nil, NewError(ErrorCodeSourceUnavailable, "the attachment is not available")
	}
	reader, meta, err := f.deps.Artifacts.Open(ctx, id)
	if err != nil {
		return nil, wrapError(ErrorCodeSourceUnavailable, "the attachment is not available", err)
	}
	if f.ownerOnly && (f.sessionID == "" || meta.SessionID != f.sessionID) {
		_ = reader.Close()
		return nil, NewError(ErrorCodeSourceUnavailable, "the attachment is not available")
	}
	file, ok := reader.(*os.File)
	if !ok {
		_ = reader.Close()
		return nil, NewError(ErrorCodeSourceUnavailable, "the attachment is not available")
	}
	return regularFile(file, meta.Filename, baseMediaType(meta.MediaType))
}

func regularFile(file *os.File, name, mediaType string) (File, error) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, NewError(ErrorCodeSourceUnavailable, "the source is not a regular file")
	}
	runes := []rune(name)
	if len(runes) > maxFileNameRunes {
		name = string(runes[:maxFileNameRunes])
	}
	return &openedFile{File: file, name: name, mediaType: mediaType, size: info.Size()}, nil
}

func baseMediaType(contentType string) string {
	head, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(head))
}

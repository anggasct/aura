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
	workspacePrefix  = "workspace:"
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

type fileIdentity struct {
	name      string
	mediaType string
	reference string
}

type openedFile struct {
	*os.File
	fileIdentity
	size int64
}

func (f *openedFile) Name() string { return f.name }

func (f *openedFile) MediaType() string { return f.mediaType }

func (f *openedFile) Reference() string { return f.reference }

func (f *openedFile) Size() int64 { return f.size }

type surfaceFiles struct {
	deps      *FileDeps
	ownerOnly bool
	hostPaths bool
	noFiles   bool
	sessionID string
}

func (r *Registry) filesFor(caller *Caller) *surfaceFiles {
	return &surfaceFiles{deps: r.files, ownerOnly: caller.Surface == SurfaceAgent, hostPaths: caller.Surface == SurfaceCLI, noFiles: caller.Surface == SurfaceChat, sessionID: caller.SessionID}
}

// refuseUnresolvedPaths fails a call closed before its handler runs when this
// platform cannot resolve a workspace source at all, so the handler never
// receives a file the kernel could not open.
func (f *surfaceFiles) refuseUnresolvedPaths(paths map[string]string) error {
	if f.deps == nil || workspace.Supported() {
		return nil
	}
	for _, source := range paths {
		switch {
		case strings.Contains(source, "://"), strings.HasPrefix(source, artifactPrefix):
			continue
		case strings.HasPrefix(source, workspacePrefix), !f.hostPaths:
			return wrapError(ErrorCodeSourceUnavailable, "the file cannot be opened", workspace.ErrUnsupported())
		}
	}
	return nil
}

func (f *surfaceFiles) Open(ctx context.Context, source string) (File, error) {
	switch {
	case f.noFiles:
		return nil, NewError(ErrorCodePolicyDenied, "this surface cannot open files")
	case strings.Contains(source, "://"):
		return nil, NewError(ErrorCodeInputInvalid, "a URL is not a file source")
	case strings.HasPrefix(source, artifactPrefix):
		return f.artifact(ctx, strings.TrimPrefix(source, artifactPrefix))
	case strings.HasPrefix(source, workspacePrefix):
		return f.workspaceFile(strings.TrimPrefix(source, workspacePrefix))
	case f.hostPaths:
		return f.hostFile(source)
	}
	return f.workspaceFile(source)
}

func (f *surfaceFiles) hostFile(location string) (File, error) {
	file, err := workspace.OpenHost(location)
	switch {
	case fault.HasCode(err, workspace.ErrorCodeSymlink):
		return nil, wrapError(ErrorCodePolicyDenied, "the path is a symbolic link", err)
	case errors.Is(err, fs.ErrNotExist):
		return nil, wrapError(ErrorCodeSourceUnavailable, "the file does not exist", err)
	case err != nil:
		return nil, wrapError(ErrorCodeSourceUnavailable, "the file cannot be opened", err)
	}
	absolute, err := filepath.Abs(location)
	if err != nil {
		_ = file.Close()
		return nil, wrapError(ErrorCodeSourceUnavailable, "the file cannot be opened", err)
	}
	return regularFile(file, &fileIdentity{name: filepath.Base(absolute), reference: absolute})
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
	clean := path.Clean(filepath.ToSlash(relative))
	return regularFile(file, &fileIdentity{name: path.Base(clean), reference: workspacePrefix + clean})
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
	return regularFile(file, &fileIdentity{name: meta.Filename, mediaType: storage.BaseMediaType(meta.MediaType), reference: artifactPrefix + id})
}

func regularFile(file *os.File, identity *fileIdentity) (File, error) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, NewError(ErrorCodeSourceUnavailable, "the source is not a regular file")
	}
	runes := []rune(identity.name)
	if len(runes) > maxFileNameRunes {
		identity.name = string(runes[:maxFileNameRunes])
	}
	return &openedFile{File: file, fileIdentity: *identity, size: info.Size()}, nil
}

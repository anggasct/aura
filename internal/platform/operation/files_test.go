package operation

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/storage"
)

type attachmentOpener map[string]storage.ArtifactMetadata

func (o attachmentOpener) Open(_ context.Context, refID string) (io.ReadCloser, storage.ArtifactMetadata, error) {
	meta, found := o[refID]
	if !found {
		return nil, storage.ArtifactMetadata{}, fault.New(storage.ErrorCodeArtifactBlobMissing, "missing")
	}
	file, err := os.CreateTemp("", "attachment-*")
	if err != nil {
		return nil, storage.ArtifactMetadata{}, err
	}
	if _, err := file.WriteString("attachment body"); err != nil {
		return nil, storage.ArtifactMetadata{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, storage.ArtifactMetadata{}, err
	}
	return file, meta, nil
}

func TestFileAuthorityOpensOnlyWhatTheCallerMayRead(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "policy.md"), []byte("policy body"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	deps := &FileDeps{Workspace: workspace, Artifacts: attachmentOpener{
		"art-own":   {SessionID: "session-1", Filename: "own.md", MediaType: "Text/Markdown; charset=utf-8"},
		"art-other": {SessionID: "session-2", Filename: "other.md", MediaType: "text/plain"},
	}}
	agent := &surfaceFiles{deps: deps, ownerOnly: true, sessionID: "session-1"}
	cli := &surfaceFiles{deps: deps, sessionID: "session-1"}
	cases := []struct {
		name     string
		files    *surfaceFiles
		source   string
		wantCode ErrorCode
		wantName string
		wantType string
	}{
		{name: "a workspace file", files: agent, source: "policy.md", wantName: "policy.md"},
		{name: "an attachment of the session", files: agent, source: "artifact:art-own", wantName: "own.md", wantType: "text/markdown"},
		{name: "an attachment of another session for the agent", files: agent, source: "artifact:art-other", wantCode: ErrorCodeSourceUnavailable},
		{name: "an attachment of another session for the owner CLI", files: cli, source: "artifact:art-other", wantName: "other.md", wantType: "text/plain"},
		{name: "an unknown attachment", files: agent, source: "artifact:art-none", wantCode: ErrorCodeSourceUnavailable},
		{name: "an empty attachment id", files: agent, source: "artifact:", wantCode: ErrorCodeSourceUnavailable},
		{name: "an escaping path", files: agent, source: "../policy.md", wantCode: ErrorCodePolicyDenied},
		{name: "an absolute path", files: agent, source: filepath.Join(workspace, "policy.md"), wantCode: ErrorCodePolicyDenied},
		{name: "a missing file", files: agent, source: "missing.md", wantCode: ErrorCodeSourceUnavailable},
		{name: "a directory", files: agent, source: "directory", wantCode: ErrorCodeSourceUnavailable},
		{name: "a URL", files: agent, source: "https://docs.example.test/a", wantCode: ErrorCodeInputInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, err := tc.files.Open(t.Context(), tc.source)

			if tc.wantCode != "" {
				if !fault.HasCode(err, tc.wantCode) {
					t.Fatalf("Open(%q) = %v, want %s", tc.source, err, tc.wantCode)
				}
				if err != nil && strings.Contains(err.Error(), workspace) {
					t.Errorf("error %q names the workspace path", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open(%q): %v", tc.source, err)
			}
			defer func() { _ = file.Close() }()
			if file.Name() != tc.wantName || file.MediaType() != tc.wantType || file.Size() == 0 {
				t.Errorf("file = %q %q %d, want %q %q", file.Name(), file.MediaType(), file.Size(), tc.wantName, tc.wantType)
			}
		})
	}
}

func TestFileAuthorityWithoutDependenciesRefuses(t *testing.T) {
	files := &surfaceFiles{ownerOnly: true, sessionID: "session-1"}
	for _, source := range []string{"policy.md", "artifact:art-own"} {
		t.Run(source, func(t *testing.T) {
			if _, err := files.Open(t.Context(), source); err == nil {
				t.Fatalf("Open(%q) succeeded without a workspace or an artifact store", source)
			}
		})
	}
}

func TestFileDepsRequireAnAbsoluteWorkspace(t *testing.T) {
	if err := (&FileDeps{Workspace: "relative/dir"}).validate(); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("validate = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if err := (&FileDeps{Workspace: t.TempDir()}).validate(); err != nil {
		t.Fatalf("validate of an absolute workspace: %v", err)
	}
	if err := (*FileDeps)(nil).validate(); err != nil {
		t.Fatalf("validate of no deps: %v", err)
	}
}

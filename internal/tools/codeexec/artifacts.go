package codeexec

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/anggasct/aura/internal/tools"
)

const (
	outputDirName    = "output"
	outputDirMode    = 0o700
	sniffBytes       = 512
	artifactKind     = "code_execute_output"
	artifactIDPrefix = "art-"
	artifactIDRandom = 8
	openOutputFlags  = os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	soleLink         = 1
)

type Refusal string

const (
	RefusalAbsent      Refusal = "absent"
	RefusalSymlink     Refusal = "symlink"
	RefusalNotRegular  Refusal = "not_regular"
	RefusalHardLink    Refusal = "hard_link"
	RefusalTooLarge    Refusal = "too_large"
	RefusalTooMany     Refusal = "too_many"
	RefusalInvalidName Refusal = "invalid_name"
	RefusalDuplicate   Refusal = "duplicate"
	RefusalQuota       Refusal = "artifact_quota"
	RefusalUnreadable  Refusal = "unreadable"
	RefusalImport      Refusal = "import_failed"
)

type ArtifactEntry struct {
	Name       string  `json:"name"`
	ArtifactID string  `json:"artifact_id"`
	Digest     string  `json:"digest"`
	MediaType  string  `json:"media_type"`
	Size       int64   `json:"size"`
	Refused    Refusal `json:"refused,omitempty"`
}

type ArtifactMeta struct {
	ID        string
	SessionID string
	Filename  string
	MediaType string
	Metadata  json.RawMessage
}

type ArtifactReceipt struct {
	ID        string
	Digest    string
	SizeBytes int64
}

type ArtifactSink interface {
	Put(ctx context.Context, content io.Reader, meta *ArtifactMeta) (ArtifactReceipt, error)
}

type provenance struct {
	sessionID string
	metadata  json.RawMessage
}

type provenanceRecord struct {
	Kind           string `json:"kind"`
	Runtime        string `json:"runtime"`
	RuntimeVersion string `json:"runtime_version"`
	RequestID      string `json:"request_id"`
}

func newProvenance(sessionID, requestID, runtime, version string) (provenance, error) {
	metadata, err := json.Marshal(provenanceRecord{Kind: artifactKind, Runtime: runtime, RuntimeVersion: version, RequestID: requestID})
	if err != nil {
		return provenance{}, wrapf(ErrorCodeRunFailed, err, "encode the artifact provenance")
	}
	return provenance{sessionID: sessionID, metadata: metadata}, nil
}

type importer struct {
	sink     ArtifactSink
	maxFiles int
	maxBytes int64
}

func (i *importer) importAll(ctx context.Context, outputDir string, names []string, source *provenance) []ArtifactEntry {
	if len(names) == 0 {
		return nil
	}
	entries := make([]ArtifactEntry, 0, len(names))
	dirErr := checkOutputDir(outputDir)
	seen := make(map[string]struct{}, len(names))
	for index, name := range names {
		refused := func(reason Refusal) ArtifactEntry { return ArtifactEntry{Name: name, Refused: reason} }
		switch {
		case index >= i.maxFiles:
			entries = append(entries, refused(RefusalTooMany))
			continue
		case tools.ValidateCodeOutputFile(name) != nil:
			entries = append(entries, refused(RefusalInvalidName))
			continue
		}
		if _, repeated := seen[name]; repeated {
			entries = append(entries, refused(RefusalDuplicate))
			continue
		}
		seen[name] = struct{}{}
		if dirErr != nil {
			entries = append(entries, refused(RefusalAbsent))
			continue
		}
		entries = append(entries, i.importOne(ctx, outputDir, name, source))
	}
	return entries
}

func checkOutputDir(outputDir string) error {
	info, err := os.Lstat(outputDir)
	if err != nil {
		return wrapf(ErrorCodeRunFailed, pathFree(err), "inspect the output directory")
	}
	if !info.IsDir() {
		return Errorf(ErrorCodeRunFailed, "the output location is not a directory")
	}
	return nil
}

func (i *importer) importOne(ctx context.Context, outputDir, name string, source *provenance) ArtifactEntry {
	entry := ArtifactEntry{Name: name}
	if reason := walkOutputPath(outputDir, name); reason != "" {
		entry.Refused = reason
		return entry
	}
	file, err := openOutput(filepath.Join(outputDir, filepath.FromSlash(name)))
	if err != nil {
		entry.Refused = openRefusal(err)
		return entry
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		entry.Refused = RefusalUnreadable
		return entry
	}
	if reason := refuseInfo(info, i.maxBytes); reason != "" {
		entry.Refused = reason
		return entry
	}
	media, content, err := sniff(file, info.Size())
	if err != nil {
		entry.Refused = RefusalUnreadable
		return entry
	}
	id, err := newArtifactID()
	if err != nil {
		entry.Refused = RefusalImport
		return entry
	}
	receipt, err := i.sink.Put(ctx, content, &ArtifactMeta{
		ID: id, SessionID: source.sessionID, Filename: path.Base(name), MediaType: media, Metadata: source.metadata,
	})
	if err != nil {
		entry.Refused = putRefusal(err)
		return entry
	}
	entry.ArtifactID, entry.Digest, entry.MediaType, entry.Size = receipt.ID, receipt.Digest, media, receipt.SizeBytes
	return entry
}

func walkOutputPath(outputDir, name string) Refusal {
	segments := strings.Split(name, "/")
	for depth := range segments {
		info, err := os.Lstat(filepath.Join(outputDir, filepath.FromSlash(strings.Join(segments[:depth+1], "/"))))
		if err != nil {
			return lstatRefusal(err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return RefusalSymlink
		}
		if depth == len(segments)-1 {
			return refuseMode(info.Mode())
		}
		if !info.IsDir() {
			return RefusalNotRegular
		}
	}
	return ""
}

func refuseMode(mode fs.FileMode) Refusal {
	switch {
	case mode&fs.ModeSymlink != 0:
		return RefusalSymlink
	case !mode.IsRegular():
		return RefusalNotRegular
	default:
		return ""
	}
}

func refuseInfo(info fs.FileInfo, maxBytes int64) Refusal {
	if reason := refuseMode(info.Mode()); reason != "" {
		return reason
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return RefusalUnreadable
	}
	if stat.Nlink != soleLink {
		return RefusalHardLink
	}
	if info.Size() > maxBytes {
		return RefusalTooLarge
	}
	return ""
}

func openOutput(target string) (*os.File, error) {
	fd, err := syscall.Open(target, openOutputFlags|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), outputDirName), nil
}

func lstatRefusal(err error) Refusal {
	if errors.Is(err, fs.ErrNotExist) {
		return RefusalAbsent
	}
	return RefusalUnreadable
}

func openRefusal(err error) Refusal {
	switch {
	case errors.Is(err, syscall.ELOOP):
		return RefusalSymlink
	case errors.Is(err, fs.ErrNotExist):
		return RefusalAbsent
	default:
		return RefusalUnreadable
	}
}

func putRefusal(err error) Refusal {
	if code, ok := CodeOf(err); ok && code == ErrorCodeArtifactQuota {
		return RefusalQuota
	}
	return RefusalImport
}

func sniff(file io.Reader, size int64) (string, io.Reader, error) {
	head := make([]byte, min(size, sniffBytes))
	if _, err := io.ReadFull(file, head); err != nil {
		return "", nil, err
	}
	media := http.DetectContentType(head)
	return media, io.LimitReader(io.MultiReader(bytes.NewReader(head), file), size), nil
}

func newArtifactID() (string, error) {
	var raw [artifactIDRandom]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", wrapf(ErrorCodeRunFailed, err, "generate an artifact id")
	}
	return artifactIDPrefix + hex.EncodeToString(raw[:]), nil
}

//go:build linux

package filesystem

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/toolbroker"
	"golang.org/x/sys/unix"
)

type fileArguments struct {
	Path      string `json:"path"`
	MaxBytes  int64  `json:"max_bytes"`
	Content   string `json:"content"`
	Overwrite bool   `json:"overwrite"`
}

type dirArguments struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}

// sysClose and sysFsync are seams so tests can inject close and fsync
// failures into the write durability path.
var (
	sysClose = unix.Close
	sysFsync = unix.Fsync
)

func New(options Options) (map[string]toolbroker.Adapter, error) {
	if err := validateOptions(&options); err != nil {
		return nil, err
	}
	return map[string]toolbroker.Adapter{
		"read_file@v1": func(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints) (toolbroker.ToolResult, error) {
			return readFile(ctx, request, options, constraints)
		},
		"write_file@v1": func(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints) (toolbroker.ToolResult, error) {
			return writeFile(ctx, request, options, constraints)
		},
		"list_dir@v1": func(ctx context.Context, request *toolbroker.ToolRequest, constraints approval.Constraints) (toolbroker.ToolResult, error) {
			return listDir(ctx, request, options, constraints)
		},
	}, nil
}

func readFile(_ context.Context, request *toolbroker.ToolRequest, options Options, _ approval.Constraints) (toolbroker.ToolResult, error) {
	var args fileArguments
	if err := decodeArguments(request, &args); err != nil {
		return toolbroker.ToolResult{}, err
	}
	if err := validateRelativePath(args.Path); err != nil {
		return toolbroker.ToolResult{}, err
	}
	limit := options.MaxFileBytes
	if args.MaxBytes > 0 && args.MaxBytes < limit {
		limit = args.MaxBytes
	}
	root, err := openWorkspace(options.Workspace)
	if err != nil {
		return toolbroker.ToolResult{}, err
	}
	defer func() { _ = unix.Close(root) }()
	fd, err := openRelative(root, args.Path, uint64(unix.O_RDONLY|unix.O_CLOEXEC))
	if err != nil {
		return toolbroker.ToolResult{}, pathError("read", args.Path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	data, err := io.ReadAll(io.LimitReader(os.NewFile(uintptr(fd), "read-file"), limit+1))
	if err != nil {
		return toolbroker.ToolResult{}, fmt.Errorf("filesystem: read file: %w", err)
	}
	truncated := int64(len(data)) > limit
	if truncated {
		data = data[:limit]
	}
	return encodeResult(fileResult{Content: string(data), Truncated: truncated})
}

func writeFile(ctx context.Context, request *toolbroker.ToolRequest, options Options, _ approval.Constraints) (toolbroker.ToolResult, error) {
	var args fileArguments
	if err := decodeArguments(request, &args); err != nil {
		return toolbroker.ToolResult{}, err
	}
	if err := validateRelativePath(args.Path); err != nil {
		return toolbroker.ToolResult{}, err
	}
	if int64(len(args.Content)) > options.MaxFileBytes {
		return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultPolicyDenied, "file content exceeds configured limit")
	}
	root, err := openWorkspace(options.Workspace)
	if err != nil {
		return toolbroker.ToolResult{}, err
	}
	defer func() { _ = unix.Close(root) }()
	parentPath, base := splitParent(args.Path)
	parent, err := openRelative(root, parentPath, uint64(unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC))
	if err != nil {
		return toolbroker.ToolResult{}, pathError("write", args.Path, err)
	}
	defer func() { _ = unix.Close(parent) }()
	if err := rejectHardLink(parent, base); err != nil {
		return toolbroker.ToolResult{}, err
	}
	if !args.Overwrite {
		fd, err := openNew(parent, base)
		if err != nil {
			return toolbroker.ToolResult{}, pathError("create", args.Path, err)
		}
		if err := writeAndSync(fd, []byte(args.Content)); err != nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(parent, base, 0)
			return toolbroker.ToolResult{}, fmt.Errorf("filesystem: write file: %w", err)
		}
		if err := sysClose(fd); err != nil {
			_ = unix.Unlinkat(parent, base, 0)
			return toolbroker.ToolResult{}, fmt.Errorf("filesystem: write file: %w", err)
		}
		if err := sysFsync(parent); err != nil {
			return toolbroker.ToolResult{}, fmt.Errorf("filesystem: sync directory: %w", err)
		}
	} else if err := atomicReplace(parent, base, []byte(args.Content)); err != nil {
		return toolbroker.ToolResult{}, pathError("replace", args.Path, err)
	}
	select {
	case <-ctx.Done():
		return toolbroker.ToolResult{}, ctx.Err()
	default:
	}
	return encodeResult(fileResult{Content: "", Truncated: false})
}

func listDir(ctx context.Context, request *toolbroker.ToolRequest, options Options, _ approval.Constraints) (toolbroker.ToolResult, error) {
	var args dirArguments
	if err := decodeArguments(request, &args); err != nil {
		return toolbroker.ToolResult{}, err
	}
	if err := validateRelativePath(args.Path); err != nil {
		return toolbroker.ToolResult{}, err
	}
	root, err := openWorkspace(options.Workspace)
	if err != nil {
		return toolbroker.ToolResult{}, err
	}
	defer func() { _ = unix.Close(root) }()
	result := directoryResult{Entries: make([]directoryEntry, 0)}
	if err := listDirectory(ctx, root, args.Path, args.Recursive, options.MaxDirEntries, &result); err != nil {
		return toolbroker.ToolResult{}, err
	}
	return encodeResult(result)
}

func listDirectory(ctx context.Context, root int, relative string, recursive bool, limit int, result *directoryResult) error {
	fd, err := openRelative(root, relative, uint64(unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC))
	if err != nil {
		return pathError("list", relative, err)
	}
	file := os.NewFile(uintptr(fd), "list-dir")
	if file == nil {
		_ = unix.Close(fd)
		return errors.New("filesystem: directory handle unavailable")
	}
	defer func() { _ = file.Close() }()
	for {
		remaining := limit - len(result.Entries)
		// Read at most the remaining budget plus one entry so truncation
		// is detectable without ever materializing the full listing. An
		// exhausted budget probes a single entry to distinguish an exact
		// fit from a truncated listing.
		readSize := 1
		if remaining > 0 {
			readSize = remaining + 1
		}
		batch, err := file.Readdir(readSize)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("filesystem: read directory: %w", err)
		}
		if len(batch) == 0 {
			return nil
		}
		if remaining <= 0 {
			result.Truncated = true
			return nil
		}
		if len(batch) > remaining {
			result.Truncated = true
			batch = batch[:remaining]
		}
		for _, entry := range batch {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			// The budget is shared with recursive descents, so it must be
			// rechecked per entry, not per batch.
			if len(result.Entries) >= limit {
				result.Truncated = true
				return nil
			}
			entryPath := entry.Name()
			if relative != "." {
				entryPath = filepath.Join(relative, entry.Name())
			}
			kind := "file"
			if entry.IsDir() {
				kind = "directory"
			} else if entry.Mode()&os.ModeSymlink != 0 {
				kind = "symlink"
			}
			result.Entries = append(result.Entries, directoryEntry{Path: entryPath, Kind: kind, Size: entry.Size()})
			if recursive && entry.IsDir() {
				if err := listDirectory(ctx, root, entryPath, true, limit, result); err != nil {
					return err
				}
			}
		}
		if result.Truncated || errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func decodeArguments(request *toolbroker.ToolRequest, target any) error {
	if request == nil {
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
	}
	if err := json.Unmarshal(request.Arguments, target); err != nil {
		return toolbroker.Errorf(toolbroker.ResultInvalidArgument, "invalid filesystem arguments")
	}
	return nil
}

func encodeResult(value any) (toolbroker.ToolResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return toolbroker.ToolResult{}, fmt.Errorf("filesystem: encode result: %w", err)
	}
	return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: data}, nil
}

func validateRelativePath(path string) error {
	if path == "" || filepath.IsAbs(path) {
		return toolbroker.Errorf(toolbroker.ResultPolicyDenied, "path must be relative to the workspace")
	}
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." {
			return toolbroker.Errorf(toolbroker.ResultPolicyDenied, "path traversal is not allowed")
		}
	}
	if filepath.Clean(path) == ".." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator)) {
		return toolbroker.Errorf(toolbroker.ResultPolicyDenied, "path traversal is not allowed")
	}
	return nil
}

func pathError(operation, path string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EEXIST) {
		return toolbroker.Errorf(toolbroker.ResultPolicyDenied, "%s path is outside the workspace", operation)
	}
	return fmt.Errorf("filesystem: %s %s: %w", operation, path, err)
}

func splitParent(path string) (dir, base string) {
	dir, base = filepath.Split(path)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	if dir == "" {
		dir = "."
	}
	return dir, base
}

func openWorkspace(path string) (int, error) {
	return unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
}

func openRelative(root int, path string, flags uint64) (int, error) {
	how := &unix.OpenHow{
		Flags:   flags,
		Mode:    0,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	}
	return unix.Openat2(root, path, how)
}

func openNew(parent int, name string) (int, error) {
	return unix.Openat2(parent, name, &unix.OpenHow{
		Flags:   unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Mode:    0o600,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_XDEV,
	})
}

func rejectHardLink(parent int, name string) error {
	fd, err := openRelative(parent, name, uint64(unix.O_PATH|unix.O_CLOEXEC))
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return pathError("inspect", name, err)
	}
	defer func() { _ = unix.Close(fd) }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("filesystem: inspect file: %w", err)
	}
	if stat.Nlink > 1 {
		return toolbroker.Errorf(toolbroker.ResultPolicyDenied, "hard-linked destinations are not writable")
	}
	return nil
}

func writeAndSync(fd int, content []byte) error {
	for len(content) > 0 {
		n, err := unix.Write(fd, content)
		if err != nil {
			return err
		}
		content = content[n:]
	}
	return sysFsync(fd)
}

func atomicReplace(parent int, name string, content []byte) error {
	tmp, err := temporaryName()
	if err != nil {
		return err
	}
	fd, err := openNew(parent, tmp)
	if err != nil {
		return err
	}
	removeTemp := true
	defer func() {
		_ = unix.Close(fd)
		if removeTemp {
			_ = unix.Unlinkat(parent, tmp, 0)
		}
	}()
	if err := writeAndSync(fd, content); err != nil {
		return err
	}
	if err := sysClose(fd); err != nil {
		return err
	}
	fd = -1
	if err := unix.Renameat(parent, tmp, parent, name); err != nil {
		return err
	}
	removeTemp = false
	return sysFsync(parent)
}

func temporaryName() (string, error) {
	var data [12]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return ".aura-write-" + hex.EncodeToString(data[:]), nil
}

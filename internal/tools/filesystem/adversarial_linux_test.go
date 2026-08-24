//go:build linux

package filesystem

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/toolbroker"
	"golang.org/x/sys/unix"
)

// TestFilesystemRejectsSymlinkChainsAndMagicLinks covers the symlink
// battery: chains whose final target escapes the workspace, internal
// symlink-to-symlink loops, final-component symlinks that stay inside the
// workspace (fail-closed by design), and /proc-derived magic links reached
// through relative components.
func TestFilesystemRejectsSymlinkChainsAndMagicLinks(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	inside := filepath.Join(workspace, "inside.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
		t.Fatalf("write inside: %v", err)
	}
	// a -> b -> ../<outside>/secret
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(workspace, "b")); err != nil {
		t.Fatalf("symlink b: %v", err)
	}
	if err := os.Symlink("b", filepath.Join(workspace, "a")); err != nil {
		t.Fatalf("symlink a: %v", err)
	}
	// loop -> loop
	if err := os.Symlink("loop", filepath.Join(workspace, "loop")); err != nil {
		t.Fatalf("symlink loop: %v", err)
	}
	// self-escape via relative symlink target
	if err := os.Symlink("../"+filepath.Base(outside)+"/secret", filepath.Join(workspace, "rel")); err != nil {
		t.Fatalf("symlink rel: %v", err)
	}
	// internal final-component symlink: inside-link -> inside.txt
	if err := os.Symlink("inside.txt", filepath.Join(workspace, "inside-link")); err != nil {
		t.Fatalf("symlink inside-link: %v", err)
	}
	// magic link through a relative component: proc-self -> /proc/self
	if err := os.Symlink("/proc/self", filepath.Join(workspace, "proc-self")); err != nil {
		t.Fatalf("symlink proc-self: %v", err)
	}
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 4096, MaxDirEntries: 100})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	read := adapters["read_file@v1"]
	for _, path := range []string{"a", "loop", "rel", "inside-link", "proc-self/fd/0", "b"} {
		request := fsRequest(t, map[string]any{"path": path})
		_, err := read(context.Background(), request, approvalConstraints())
		if class := classOf(err); class != toolbroker.ResultPolicyDenied {
			t.Errorf("read %q class = %q, err = %v", path, class, err)
		}
	}
	write := adapters["write_file@v1"]
	for _, path := range []string{"a", "loop", "rel", "inside-link", "proc-self/maps"} {
		request := fsRequest(t, map[string]any{"path": path, "content": "x"})
		_, err := write(context.Background(), request, approvalConstraints())
		if class := classOf(err); class != toolbroker.ResultPolicyDenied {
			t.Errorf("write %q class = %q, err = %v", path, class, err)
		}
	}
}

// TestFilesystemRejectsTraversalSpellings covers absolute paths, dot-dot
// traversal including backslash separators and dot-encoding tricks, for
// every tool.
func TestFilesystemRejectsTraversalSpellings(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 1024, MaxDirEntries: 10})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	outsideRel := "../" + filepath.Base(outside)
	paths := []string{
		filepath.Join(outside, "secret"),
		"../secret",
		outsideRel + "/secret",
		"..\\..\\secret",
		"sub/../../" + filepath.Base(outside) + "/secret",
		"./../" + filepath.Base(outside) + "/secret",
		"/etc/passwd",
	}
	for name, adapter := range adapters {
		for _, path := range paths {
			var request = fsRequest(t, map[string]any{"path": path})
			_, err := adapter(context.Background(), request, approvalConstraints())
			if class := classOf(err); class != toolbroker.ResultPolicyDenied {
				t.Errorf("%s %q class = %q, err = %v", name, path, class, err)
			}
		}
	}
}

// TestFilesystemMapsEscapeErrnosToPolicyDenied pins the errno mapping used
// by the openat2 resolve flags: symlink resolution (ELOOP), mount-boundary
// crossing (EXDEV), and privileged denial (EPERM) must all surface as
// policy_denied, never as a raw execution failure.
func TestFilesystemMapsEscapeErrnosToPolicyDenied(t *testing.T) {
	for _, errno := range []error{unix.ELOOP, unix.EXDEV, unix.EPERM, unix.EEXIST} {
		err := pathError("read", "some/path", errno)
		if class := classOf(err); class != toolbroker.ResultPolicyDenied {
			t.Errorf("pathError(%v) class = %q, err = %v", errno, class, err)
		}
	}
	if class := classOf(pathError("read", "some/path", unix.ENOENT)); class == toolbroker.ResultPolicyDenied {
		t.Errorf("ENOENT must not map to policy_denied (missing files are execution failures, not escapes)")
	}
}

// TestFilesystemRaceAgainstEntrySwapping runs the tools in a loop while an
// attacker goroutine swaps a workspace entry between a real file inside the
// workspace and a symlink pointing at a canary outside it. Every read must
// return either the inside content or a typed error — never canary content —
// and every write must never touch the canary.
func TestFilesystemRaceAgainstEntrySwapping(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	canaryPath := filepath.Join(outside, "canary")
	if err := os.WriteFile(canaryPath, []byte("CANARY"), 0o600); err != nil {
		t.Fatalf("write canary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "swap"), []byte("INSIDE"), 0o600); err != nil {
		t.Fatalf("write inside: %v", err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "dir", "swap"), []byte("INSIDE"), 0o600); err != nil {
		t.Fatalf("write nested: %v", err)
	}
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 1024, MaxDirEntries: 100})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	read := adapters["read_file@v1"]
	write := adapters["write_file@v1"]

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	defer func() {
		stop.Store(true)
		wg.Wait()
	}()
	go func() {
		defer wg.Done()
		usingDir := false
		for !stop.Load() {
			name := "swap"
			if usingDir {
				name = filepath.Join("dir", "swap")
			}
			usingDir = !usingDir
			full := filepath.Join(workspace, name)
			_ = os.Remove(full)
			if err := os.Symlink(canaryPath, full); err != nil && !os.IsExist(err) {
				return
			}
			_ = os.Remove(full)
			if err := os.WriteFile(full, []byte("INSIDE"), 0o600); err != nil {
				return
			}
		}
	}()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, path := range []string{"swap", "dir/swap"} {
			request := fsRequest(t, map[string]any{"path": path})
			result, err := read(context.Background(), request, approvalConstraints())
			if err != nil {
				if class := classOf(err); class != toolbroker.ResultPolicyDenied && class != "" {
					t.Errorf("read %q class = %q, err = %v", path, class, err)
				}
				continue
			}
			var decoded fileResult
			if err := json.Unmarshal(result.Output, &decoded); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if decoded.Content == "CANARY" {
				t.Fatalf("read %q returned canary content through a swapped symlink", path)
			}
			// The attacker goroutine truncates and refills the inside
			// file and the reader loop itself writes "WROTE", so both are
			// legal in-flight states; the invariant under attack is that
			// canary content never appears.
			if decoded.Content != "INSIDE" && decoded.Content != "WROTE" && decoded.Content != "" {
				t.Fatalf("read %q content = %q, want INSIDE, WROTE, or empty", path, decoded.Content)
			}
			request = fsRequest(t, map[string]any{"path": path, "content": "WROTE", "overwrite": true})
			if _, err := write(context.Background(), request, approvalConstraints()); err != nil {
				if class := classOf(err); class != toolbroker.ResultPolicyDenied && class != "" {
					t.Errorf("write %q class = %q, err = %v", path, class, err)
				}
			}
		}
	}
	stop.Store(true)
	wg.Wait()

	if got, err := os.ReadFile(canaryPath); err != nil || string(got) != "CANARY" {
		t.Fatalf("canary was modified: %q, %v", got, err)
	}
	steps, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("read outside dir: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("outside dir entries = %d (%v), want only the canary", len(steps), steps)
	}
}

// TestFilesystemRaceAgainstDirectoryRenaming renames workspace directories
// (including their parents) while list_dir and read_file run concurrently.
// Operations must complete against the pinned tree or fail with a typed
// error; no hang, no outside read.
func TestFilesystemRaceAgainstDirectoryRenaming(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	for i := range 4 {
		dir := filepath.Join(workspace, fmt.Sprintf("d%d", i))
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte(fmt.Sprintf("CONTENT%d", i)), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}
	adapters, err := New(Options{Workspace: workspace, MaxFileBytes: 1024, MaxDirEntries: 100})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	read := adapters["read_file@v1"]
	list := adapters["list_dir@v1"]

	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	defer func() {
		stop.Store(true)
		wg.Wait()
	}()
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i = (i + 1) % 4 {
			from := filepath.Join(workspace, fmt.Sprintf("d%d", i))
			to := filepath.Join(workspace, fmt.Sprintf("d%d", (i+1)%4))
			_ = os.Rename(from, to+".tmp")
			_ = os.Rename(to+".tmp", to)
			_ = os.Remove(filepath.Join(workspace, "sub"))
			_ = os.Symlink(filepath.Join(outside, "secret"), filepath.Join(workspace, "sub"))
			_ = os.Remove(filepath.Join(workspace, "sub"))
			_ = os.Mkdir(filepath.Join(workspace, "sub"), 0o755)
		}
	}()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		for i := range 4 {
			request := fsRequest(t, map[string]any{"path": fmt.Sprintf("d%d/sub/f.txt", i)})
			result, err := read(context.Background(), request, approvalConstraints())
			if err == nil {
				var decoded fileResult
				if err := json.Unmarshal(result.Output, &decoded); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if decoded.Content == "secret" {
					t.Fatal("read returned outside content after a directory race")
				}
			} else if class := classOf(err); class == toolbroker.ResultPolicyDenied || class == "" {
				// denied or plain I/O failure (ENOENT etc.) — both fine
			} else {
				t.Errorf("read class = %q, err = %v", class, err)
			}
			request = fsRequest(t, map[string]any{"path": fmt.Sprintf("d%d", i), "recursive": true})
			if _, err := list(context.Background(), request, approvalConstraints()); err != nil {
				if class := classOf(err); class != toolbroker.ResultPolicyDenied && class != "" {
					t.Errorf("list class = %q, err = %v", class, err)
				}
			}
		}
	}
	stop.Store(true)
	wg.Wait()

	if got, err := os.ReadFile(filepath.Join(outside, "secret")); err != nil || string(got) != "secret" {
		t.Fatalf("outside file changed: %q, %v", got, err)
	}
}

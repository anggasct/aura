package codeexec

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	holderEnv     = "AURA_CODEEXEC_RUNDIR_HOLDER"
	holderReady   = "ready"
	holderTimeout = 60 * time.Second
)

func init() {
	root := os.Getenv(holderEnv)
	if root == "" {
		return
	}
	dir, err := (&Runner{runRoot: root}).newRunDir()
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(filepath.Join(dir.outputDir(), "left.txt"), []byte("left"), 0o600); err != nil {
		os.Exit(3)
	}
	if _, err := os.Stdout.WriteString(holderReady + "\n"); err != nil {
		os.Exit(4)
	}
	_, _ = os.Stdin.Read(make([]byte, 1))
	os.Exit(0)
}

func startHolder(t *testing.T, root string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), holderTimeout)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), holderEnv+"="+root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != holderReady {
		t.Fatalf("the holder never reported a locked run directory: %q, %v", line, err)
	}
	return cmd
}

func runDirNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func sweeper(t *testing.T, root string) *Runner {
	t.Helper()
	return &Runner{runRoot: root}
}

func TestSweepSparesALockedRunAndRemovesACrashedOwnersDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	holder := startHolder(t, root)
	runner := sweeper(t, root)

	removed, err := runner.SweepStale(t.Context())
	if err != nil || removed != 0 {
		t.Fatalf("SweepStale with a live owner = %d, %v; want 0, nil", removed, err)
	}
	live := runDirNames(t, root)
	if len(live) != 1 {
		t.Fatalf("run directories = %v, want the live one", live)
	}
	if _, err := os.Stat(filepath.Join(root, live[0], outputDirName, "left.txt")); err != nil {
		t.Fatalf("the sweep touched a live run: %v", err)
	}

	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = holder.Wait()

	removed, err = runner.SweepStale(t.Context())
	if err != nil || removed != 1 {
		t.Fatalf("SweepStale after the crash = %d, %v; want 1, nil", removed, err)
	}
	if left := runDirNames(t, root); len(left) != 0 {
		t.Fatalf("the sweep left %v", left)
	}
}

func TestRunDirIsLockedUntilReleased(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	dir, err := sweeper(t, root).newRunDir()
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.Open(dir.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if err := lockDir(other); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("a second descriptor locked a live run directory: %v", err)
	}
	info, err := os.Stat(dir.outputDir())
	if err != nil || !info.IsDir() || info.Mode().Perm() != outputDirMode {
		t.Fatalf("output directory = %v, %v", info, err)
	}
	dir.release()
	if _, err := os.Stat(dir.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release left the run directory: %v", err)
	}
}

func TestSweepIgnoresEntriesThatAreNotRunDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	if err := os.MkdirAll(root, runDirMode); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, create := range map[string]func(string) error{
		"run-file":    func(p string) error { return os.WriteFile(p, nil, 0o600) },
		"run-link":    func(p string) error { return os.Symlink(outside, p) },
		"other-dir":   func(p string) error { return os.Mkdir(p, runDirMode) },
		"runs-nested": func(p string) error { return os.Mkdir(p, runDirMode) },
	} {
		if err := create(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := sweeper(t, root).SweepStale(t.Context())
	if err != nil || removed != 0 {
		t.Fatalf("SweepStale = %d, %v; want 0, nil", removed, err)
	}
	if got := len(runDirNames(t, root)); got != 4 {
		t.Fatalf("entries after the sweep = %d, want 4", got)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatalf("the sweep followed a link out of the run root: %v", err)
	}
}

func TestSweepRemovesADirectoryTheProgramMadeUnreadable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	stale := filepath.Join(root, runDirPrefix+"stale")
	closed := filepath.Join(stale, "closed")
	if err := os.MkdirAll(filepath.Join(closed, "inner"), runDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(closed, 0); err != nil {
		t.Fatal(err)
	}
	removed, err := sweeper(t, root).SweepStale(t.Context())
	if err != nil || removed != 1 {
		t.Fatalf("SweepStale = %d, %v; want 1, nil", removed, err)
	}
	if _, err := os.Lstat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the unreadable run directory survived: %v", err)
	}
}

func TestSweepWithoutARunRootRemovesNothing(t *testing.T) {
	removed, err := sweeper(t, filepath.Join(t.TempDir(), "missing")).SweepStale(t.Context())
	if err != nil || removed != 0 {
		t.Fatalf("SweepStale = %d, %v; want 0, nil", removed, err)
	}
}

func TestSweepStopsWhenCancelled(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	if err := os.MkdirAll(filepath.Join(root, runDirPrefix+"stale"), runDirMode); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	removed, err := sweeper(t, root).SweepStale(ctx)
	if !hasErrorCode(err, ErrorCodeRunFailed) || removed != 0 {
		t.Fatalf("SweepStale = %d, %v; want run_failed and nothing removed", removed, err)
	}
}

func TestConcurrentSweepsNeverRemoveARunThatIsBeingCreated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	runner := sweeper(t, root)
	const creators, rounds = 4, 40
	stop := make(chan struct{})
	var sweeping sync.WaitGroup
	sweeping.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = runner.SweepStale(t.Context())
			}
		}
	})
	failures := make(chan string, creators*rounds)
	var creating sync.WaitGroup
	for range creators {
		creating.Go(func() {
			for range rounds {
				dir, err := runner.newRunDir()
				if err != nil {
					failures <- err.Error()
					return
				}
				if _, err := os.Stat(dir.outputDir()); err != nil {
					failures <- "a live run directory was swept"
				}
				dir.release()
			}
		})
	}
	creating.Wait()
	close(stop)
	sweeping.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if left := runDirNames(t, root); len(left) != 0 {
		t.Fatalf("directories left behind: %v", left)
	}
}

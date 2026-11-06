//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/sys/unix"
)

func interpreterLimits() Limits {
	return Limits{
		MemoryBytes: 1 << 28, CPUTime: 5 * time.Second, MaxOutputBytes: 1 << 16, MaxOpenFiles: 64,
		MaxProcesses: 32, FileBytes: 1 << 20, Timeout: 5 * time.Second, RunDirBytes: 1 << 24,
	}
}

func TestInterpreterProfileRequiresEveryBound(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Limits)
	}{
		{"memory", func(l *Limits) { l.MemoryBytes = 0 }},
		{"processes", func(l *Limits) { l.MaxProcesses = 0 }},
		{"cpu below one second", func(l *Limits) { l.CPUTime = time.Second - 1 }},
		{"file size", func(l *Limits) { l.FileBytes = 0 }},
		{"open files", func(l *Limits) { l.MaxOpenFiles = 0 }},
		{"output", func(l *Limits) { l.MaxOutputBytes = 0 }},
		{"run directory", func(l *Limits) { l.RunDirBytes = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limits := interpreterLimits()
			tc.mutate(&limits)
			spec := Spec{Profile: ProfileInterpreter, WorkingDir: t.TempDir(), Limits: limits}
			if err := spec.validate(); !hasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("validate = %v, want invalid_argument", err)
			}
			if _, err := Run(t.Context(), &spec, "true"); !hasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("Run = %v, want invalid_argument", err)
			}
		})
	}
	t.Run("complete limits", func(t *testing.T) {
		spec := Spec{Profile: ProfileInterpreter, WorkingDir: t.TempDir(), Limits: interpreterLimits()}
		if err := spec.validate(); err != nil {
			t.Fatalf("validate = %v", err)
		}
	})
	for _, profile := range []Profile{ProfileBase, ProfileConnector} {
		t.Run(fmt.Sprintf("%q needs none of them", profile), func(t *testing.T) {
			spec := Spec{Profile: profile, WorkingDir: t.TempDir(), Limits: Limits{Timeout: time.Second}}
			if err := spec.validate(); err != nil {
				t.Fatalf("validate = %v", err)
			}
		})
	}
}

func TestInterpreterProfileKeepsTheWorkingDirectoryWritable(t *testing.T) {
	if ProfileInterpreter.workingDirReadOnly() || ProfileInterpreter.scratchDir() {
		t.Error("the interpreter profile runs in a writable run directory and gets no scratch directory")
	}
}

func TestComputeRlimitsPerProfile(t *testing.T) {
	limits := Limits{CPUTime: 7 * time.Second, FileBytes: 1 << 20, MaxOpenFiles: 64, MaxProcesses: 16}
	find := func(settings []rlimitSetting, resource int) (rlimitSetting, bool) {
		for _, setting := range settings {
			if setting.resource == resource {
				return setting, true
			}
		}
		return rlimitSetting{}, false
	}
	t.Run("interpreter cpu soft limit sits below the hard limit", func(t *testing.T) {
		cpu, ok := find(computeRlimits(ProfileInterpreter, &limits), unix.RLIMIT_CPU)
		if !ok || cpu.soft != 7 || cpu.hard != 8 {
			t.Fatalf("RLIMIT_CPU = %+v, want soft 7 and hard 8", cpu)
		}
	})
	for _, profile := range []Profile{ProfileBase, ProfileConnector} {
		t.Run(fmt.Sprintf("%q cpu limits stay equal", profile), func(t *testing.T) {
			cpu, ok := find(computeRlimits(profile, &limits), unix.RLIMIT_CPU)
			if !ok || cpu.soft != 7 || cpu.hard != 7 {
				t.Fatalf("RLIMIT_CPU = %+v, want soft and hard 7", cpu)
			}
		})
	}
	t.Run("the process rlimit is skipped on request", func(t *testing.T) {
		skipped := limits
		skipped.NoProcessRlimit = true
		if _, ok := find(computeRlimits(ProfileInterpreter, &skipped), unix.RLIMIT_NPROC); ok {
			t.Fatal("RLIMIT_NPROC must stay unset when NoProcessRlimit is true")
		}
		if _, ok := find(computeRlimits(ProfileInterpreter, &skipped), unix.RLIMIT_FSIZE); !ok {
			t.Fatal("the other bounds must still apply")
		}
	})
	t.Run("the process rlimit applies by default", func(t *testing.T) {
		nproc, ok := find(computeRlimits(ProfileInterpreter, &limits), unix.RLIMIT_NPROC)
		if !ok || nproc.soft != 16 || nproc.hard != 16 {
			t.Fatalf("RLIMIT_NPROC = %+v, want 16", nproc)
		}
	})
}

func TestLandlockRuntimeRootsStayTheSameForBaseAndConnector(t *testing.T) {
	pinned := []string{"/usr", "/lib", "/lib64", "/bin", "/sbin"}
	if got := baseRuntimeRoots(); !slices.Equal(got, pinned) {
		t.Fatalf("baseRuntimeRoots = %v, want %v", got, pinned)
	}
	var want []string
	for _, root := range pinned {
		if info, err := os.Stat(root); err == nil && info.IsDir() {
			want = append(want, root)
		}
	}
	for _, profile := range []Profile{ProfileBase, ProfileConnector} {
		t.Run(fmt.Sprintf("%q", profile), func(t *testing.T) {
			if got := landlockRuntimeRoots(profile); !slices.Equal(got, want) {
				t.Fatalf("landlockRuntimeRoots = %v, want %v", got, want)
			}
		})
	}
}

func TestLandlockInterpreterRootsAreNarrow(t *testing.T) {
	pinned := []string{"/usr/lib/x86_64-linux-gnu", "/lib/x86_64-linux-gnu", "/usr/lib64", "/lib64"}
	if got := interpreterRuntimeRoots(); !slices.Equal(got, pinned) {
		t.Fatalf("interpreterRuntimeRoots = %v, want %v", got, pinned)
	}
	closed := []string{"/usr", "/usr/bin", "/usr/lib", "/lib", "/bin", "/sbin", "/etc", "/proc", "/home", "/dev", "/tmp"}
	for _, root := range landlockRuntimeRoots(ProfileInterpreter) {
		if slices.Contains(closed, root) {
			t.Errorf("the interpreter profile must not grant %s", root)
		}
		if !slices.Contains(pinned, root) {
			t.Errorf("the interpreter profile grants the unexpected root %s", root)
		}
	}
}

func TestLandlockDeviceRules(t *testing.T) {
	cases := []struct {
		name    string
		profile Profile
		abi     int
		want    []landlockFileRule
	}{
		{"base has none", ProfileBase, 4, nil},
		{"connector has none", ProfileConnector, 4, nil},
		{"interpreter without truncate rights", ProfileInterpreter, 2, []landlockFileRule{
			{path: "/dev/urandom", access: unix.LANDLOCK_ACCESS_FS_READ_FILE},
			{path: "/dev/null", access: unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE},
		}},
		{"interpreter with truncate rights", ProfileInterpreter, 3, []landlockFileRule{
			{path: "/dev/urandom", access: unix.LANDLOCK_ACCESS_FS_READ_FILE},
			{path: "/dev/null", access: unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := landlockDeviceRules(tc.profile, tc.abi); !slices.Equal(got, tc.want) {
				t.Fatalf("landlockDeviceRules = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLandlockFileAccessDropsDirectoryRights(t *testing.T) {
	for abi := 1; abi <= 3; abi++ {
		t.Run(fmt.Sprintf("abi %d", abi), func(t *testing.T) {
			got := landlockFileAccess(landlockWriteMask(abi))
			if got&(unix.LANDLOCK_ACCESS_FS_READ_DIR|unix.LANDLOCK_ACCESS_FS_REMOVE_FILE|unix.LANDLOCK_ACCESS_FS_REMOVE_DIR|
				unix.LANDLOCK_ACCESS_FS_MAKE_REG|unix.LANDLOCK_ACCESS_FS_MAKE_DIR|unix.LANDLOCK_ACCESS_FS_MAKE_SYM|unix.LANDLOCK_ACCESS_FS_REFER) != 0 {
				t.Fatalf("file access %#x keeps directory rights", got)
			}
			if got&(unix.LANDLOCK_ACCESS_FS_READ_FILE|unix.LANDLOCK_ACCESS_FS_WRITE_FILE|unix.LANDLOCK_ACCESS_FS_EXECUTE) == 0 {
				t.Fatalf("file access %#x lost the file rights", got)
			}
		})
	}
	if got := landlockFileAccess(landlockReadMask); got != unix.LANDLOCK_ACCESS_FS_EXECUTE|unix.LANDLOCK_ACCESS_FS_READ_FILE {
		t.Fatalf("read mask on a file = %#x, want execute and read only", got)
	}
}

func TestCgroupGroupKillAndEvents(t *testing.T) {
	host, tree := newFixture(t, fixtureOwn, undelegatedUnit(strconv.Itoa(fixturePID)))
	if _, err := host.prepare(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	cg, err := host.newCgroup(&Limits{MemoryBytes: 2 << 20, MaxProcesses: 8})
	if err != nil {
		t.Fatalf("newCgroup: %v", err)
	}
	if err := cg.killAsGroup(); err != nil {
		t.Fatalf("killAsGroup: %v", err)
	}
	if got := readFixture(t, tree, cg.name+"/memory.oom.group"); got != "1" {
		t.Fatalf("memory.oom.group = %q, want 1", got)
	}
	cases := []struct {
		name   string
		files  map[string]string
		oom    int64
		pidMax int64
	}{
		{"no event files", nil, 0, 0},
		{"both counters", map[string]string{
			"memory.events": "low 0\nhigh 0\nmax 3\noom 2\noom_kill 2\noom_group_kill 1\n",
			"pids.events":   "max 5\n",
		}, 2, 5},
		{"only the oom kill counter counts", map[string]string{"memory.events": "oom 4\nmax 9\n", "pids.events": "max 0\n"}, 0, 0},
		{"malformed values", map[string]string{"memory.events": "oom_kill many\n", "pids.events": "max -3\n"}, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"memory.events", "pids.events"} {
				_ = os.Remove(filepath.Join(tree.mount, filepath.FromSlash(cg.name), name))
			}
			for name, content := range tc.files {
				if err := os.WriteFile(filepath.Join(tree.mount, filepath.FromSlash(cg.name), name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := cg.events(); got.oomKills != tc.oom || got.pidsMax != tc.pidMax {
				t.Fatalf("events = %+v, want oom_kill %d and max %d", got, tc.oom, tc.pidMax)
			}
		})
	}
}

func TestRunDirMeasurementSumsAllocatedBlocks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	sparse, err := os.Create(filepath.Join(dir, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sparse.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	if err := sparse.Close(); err != nil {
		t.Fatal(err)
	}
	usage := measureRunDir(t.Context(), dir, defaultRunDirBounds())
	if usage.bytes < 1<<20 || usage.bytes > 1<<24 {
		t.Fatalf("measured %d bytes, want the 1 MiB file and almost nothing for the sparse one", usage.bytes)
	}
	if usage.largest != 1<<30 {
		t.Fatalf("largest file = %d, want the apparent size %d", usage.largest, 1<<30)
	}
	if usage.exceeds(1<<24) || !usage.exceeds(1<<19) {
		t.Fatalf("exceeds is wrong for %+v", usage)
	}
}

func TestRunDirMeasurementIsBounded(t *testing.T) {
	t.Run("entry cap", func(t *testing.T) {
		dir := t.TempDir()
		for i := range 20 {
			if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		usage := measureRunDir(t.Context(), dir, runDirBounds{walk: time.Minute, entries: 10})
		if !usage.capped || usage.entries != 10 || !usage.exceeds(1<<62) {
			t.Fatalf("usage = %+v, want a capped walk that counts as exceeded", usage)
		}
	})
	t.Run("an exhausted walk budget counts as exceeded", func(t *testing.T) {
		dir := t.TempDir()
		for i := range 20 {
			if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		usage := measureRunDir(t.Context(), dir, runDirBounds{walk: time.Nanosecond, entries: 1000})
		if !usage.capped || !usage.exceeds(1<<62) {
			t.Fatalf("usage = %+v, want an unfinished walk to fail closed", usage)
		}
	})
	t.Run("cancelled context stops the walk", func(t *testing.T) {
		dir := t.TempDir()
		for i := range 20 {
			if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(i)), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		usage := measureRunDir(ctx, dir, defaultRunDirBounds())
		if usage.entries != 0 || usage.capped {
			t.Fatalf("a cancelled walk = %+v, want nothing visited and no verdict", usage)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		if usage := measureRunDir(t.Context(), filepath.Join(t.TempDir(), "gone"), defaultRunDirBounds()); usage != (runDirUsage{}) {
			t.Fatalf("usage of a missing directory = %+v", usage)
		}
	})
	t.Run("an unreadable directory does not hide later entries", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "a"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, "a"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "a"), 0o700) })
		if err := os.WriteFile(filepath.Join(dir, "z"), make([]byte, 1<<20), 0o600); err != nil {
			t.Fatal(err)
		}
		if usage := measureRunDir(t.Context(), dir, defaultRunDirBounds()); usage.bytes < 1<<20 {
			t.Fatalf("usage = %+v, want the file after the unreadable directory counted", usage)
		}
	})
	t.Run("symlinks are not followed", func(t *testing.T) {
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "big"), make([]byte, 1<<20), 0o600); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
		if usage := measureRunDir(t.Context(), dir, defaultRunDirBounds()); usage.bytes >= 1<<20 {
			t.Fatalf("the walk followed a symlink out of the run directory: %+v", usage)
		}
	})
}

func TestRunDirWatcherSignalsOnlyAboveTheLimit(t *testing.T) {
	t.Run("above the limit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			dir := t.TempDir()
			watcher := startRunDirWatcher(t.Context(), dir, 1<<20)
			defer watcher.stop()
			if err := os.WriteFile(filepath.Join(dir, "big"), make([]byte, 4<<20), 0o600); err != nil {
				t.Fatal(err)
			}
			<-watcher.exceeded
		})
	})
	t.Run("under the limit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			dir := t.TempDir()
			watcher := startRunDirWatcher(t.Context(), dir, 1<<30)
			time.Sleep(10 * runDirSampleInterval)
			synctest.Wait()
			watcher.stop()
			select {
			case <-watcher.exceeded:
				t.Fatal("the watcher fired under the limit")
			default:
			}
		})
	})
}

func TestObservedResultFields(t *testing.T) {
	run := func(t *testing.T, script string) (*exec.Cmd, error) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "sh", "-c", script)
		err := cmd.Run()
		if cmd.ProcessState == nil {
			t.Fatal("the helper process did not run")
		}
		return cmd, err
	}
	cases := []struct {
		name   string
		script string
		events cgroupEvents
		end    runEnd
		want   Result
	}{
		{name: "clean exit", script: "exit 0", want: Result{}},
		{name: "exit code", script: "exit 3", want: Result{ExitCode: 3}},
		{name: "signal death", script: "kill -KILL $$", want: Result{ExitCode: -1, Signal: int(syscall.SIGKILL)}},
		{name: "oom kill", script: "exit 0", events: cgroupEvents{oomKills: 1}, want: Result{OOMKilled: true}},
		{name: "pid limit", script: "exit 0", events: cgroupEvents{pidsMax: 2}, want: Result{PidsLimited: true}},
		{name: "disk and file limits", script: "exit 0", end: runEnd{diskLimited: true, fileLimited: true}, want: Result{DiskLimited: true, FileLimited: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, runErr := run(t, tc.script)
			var stdout, stderr limitedBuffer
			got := exitedResult(&stdout, &stderr, cmd, tc.events, tc.end, runErr)
			tc.want.CPUTime = got.CPUTime
			if got != tc.want {
				t.Fatalf("result = %+v, want %+v", got, tc.want)
			}
		})
	}
	t.Run("terminated and timed out", func(t *testing.T) {
		cmd, _ := run(t, "kill -KILL $$")
		stdout, stderr := limitedBuffer{limit: 4}, limitedBuffer{}
		_, _ = stdout.Write([]byte("abcdefgh"))
		got := terminatedResult(&stdout, &stderr, cmd, cgroupEvents{}, runEnd{terminated: true, timedOut: true})
		if !got.Terminated || !got.TimedOut || !got.StdoutTruncated || got.StderrTruncated || !got.Truncated || got.Stdout != "abcd" || got.Signal != int(syscall.SIGKILL) {
			t.Fatalf("result = %+v", got)
		}
	})
	t.Run("terminated by cancellation is not a timeout", func(t *testing.T) {
		cmd, _ := run(t, "kill -KILL $$")
		var stdout, stderr limitedBuffer
		got := terminatedResult(&stdout, &stderr, cmd, cgroupEvents{}, runEnd{terminated: true})
		if !got.Terminated || got.TimedOut {
			t.Fatalf("result = %+v", got)
		}
	})
}

func TestRunWithACancelledContextCreatesNoCgroup(t *testing.T) {
	host, tree := newFixture(t, fixtureOwn, undelegatedUnit(strconv.Itoa(fixturePID)))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	spec := Spec{Profile: ProfileInterpreter, WorkingDir: t.TempDir(), Limits: interpreterLimits()}
	if _, err := runWithCgroups(ctx, host, &spec, "true"); !hasCode(err, ErrorCodeSandboxUnavailable) {
		t.Fatalf("runWithCgroups = %v, want sandbox_unavailable", err)
	}
	if got := entriesWithPrefix(t, tree, fixtureOwn, sandboxCgroupStem); len(got) != 0 {
		t.Fatalf("a cancelled run created cgroups: %v", got)
	}
}

func TestCgroupGroupKillFailsClosed(t *testing.T) {
	host, tree := newFixture(t, fixtureOwn, undelegatedUnit(strconv.Itoa(fixturePID)))
	if _, err := host.prepare(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	cg, err := host.newCgroup(&Limits{MemoryBytes: 2 << 20, MaxProcesses: 8})
	if err != nil {
		t.Fatalf("newCgroup: %v", err)
	}
	tree.failOn = cg.name + "/memory.oom.group"
	if err := cg.killAsGroup(); !hasCode(err, ErrorCodeSandboxInitFailed) {
		t.Fatalf("killAsGroup = %v, want sandbox_init_failed", err)
	}
}

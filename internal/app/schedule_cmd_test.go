package app

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/modules/schedule"
)

func writeScheduleCLIConfig(t *testing.T, dataDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 1\nstorage:\n  path: " + dataDir + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	seedConfiguredStorage(t, path)
	return path
}

func cronCommand(gf *GlobalFlags) *cobra.Command {
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return NewUsageError(err) })
	mountOperationCommands(root, gf)
	cmd, _, _ := root.Find([]string{"cron"})
	root.RemoveCommand(cmd)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return NewUsageError(err) })
	return cmd
}

func runScheduleCommand(t *testing.T, gf *GlobalFlags, args ...string) (string, error) {
	t.Helper()
	restoreDefaultLogger(t)
	cmd := cronCommand(gf)
	out := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func addScheduleJob(t *testing.T, gf *GlobalFlags, extra ...string) string {
	t.Helper()
	args := []string{"add",
		"--name", "nightly", "--schedule", "0 21 * * *", "--timezone", "UTC",
		"--channel", "discord", "--destination", "default", "--prompt", "summarize",
	}
	args = append(args, extra...)
	out, err := runScheduleCommand(t, gf, args...)
	if err != nil {
		t.Fatalf("cron add: %v\n%s", err, out)
	}
	_, id, found := strings.Cut(out, "id: ")
	if !found {
		t.Fatalf("add output lacks id:\n%s", out)
	}
	return strings.TrimSpace(strings.SplitN(id, "\n", 2)[0])
}

func TestScheduleAddListRunsRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	gf := &GlobalFlags{ConfigPath: writeScheduleCLIConfig(t, dataDir)}
	id := addScheduleJob(t, gf)

	out, err := runScheduleCommand(t, gf, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"nightly", "0 21 * * *", "active"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, id) {
		t.Errorf("list output lacks job id %q:\n%s", id, out)
	}

	out, err = runScheduleCommand(t, gf, "runs", id)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if !strings.Contains(out, "SCHEDULED_FOR") {
		t.Errorf("runs header missing:\n%s", out)
	}
}

func TestScheduleAddRejectsInvalid(t *testing.T) {
	dataDir := t.TempDir()
	gf := &GlobalFlags{ConfigPath: writeScheduleCLIConfig(t, dataDir)}
	if _, err := runScheduleCommand(t, gf, "add", "--name", "x"); err == nil {
		t.Error("add without flags accepted")
	}
	if _, err := runScheduleCommand(t, gf, "add",
		"--name", "x", "--schedule", "nope", "--timezone", "UTC",
		"--channel", "discord", "--destination", "default", "--prompt", "p"); err == nil {
		t.Error("bad expression accepted")
	}
}

func TestSchedulePauseResumeDelete(t *testing.T) {
	dataDir := t.TempDir()
	gf := &GlobalFlags{ConfigPath: writeScheduleCLIConfig(t, dataDir)}
	id := addScheduleJob(t, gf)

	if out, err := runScheduleCommand(t, gf, "pause", id); err != nil {
		t.Fatalf("pause: %v\n%s", err, out)
	} else if !strings.Contains(out, "paused") {
		t.Errorf("pause output:\n%s", out)
	}
	if out, err := runScheduleCommand(t, gf, "list"); err != nil {
		t.Fatalf("list: %v", err)
	} else if !strings.Contains(out, "paused") {
		t.Errorf("paused state not listed:\n%s", out)
	}
	if out, err := runScheduleCommand(t, gf, "resume", id); err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	} else if !strings.Contains(out, "active") {
		t.Errorf("resume output:\n%s", out)
	}
	if out, err := runScheduleCommand(t, gf, "delete", id); err != nil {
		t.Fatalf("delete: %v\n%s", err, out)
	} else if !strings.Contains(out, "deleted") {
		t.Errorf("delete output:\n%s", out)
	}
	if _, err := runScheduleCommand(t, gf, "pause", "missing"); err == nil {
		t.Error("pause of missing job accepted")
	}
}

func TestScheduleRunNowStartsRun(t *testing.T) {
	dataDir := t.TempDir()
	gf := &GlobalFlags{ConfigPath: writeScheduleCLIConfig(t, dataDir)}
	id := addScheduleJob(t, gf)

	out, err := runScheduleCommand(t, gf, "run-now", id)
	if err != nil {
		t.Fatalf("run-now: %v\n%s", err, out)
	}
	if !strings.Contains(out, "occurrence:") {
		t.Errorf("run-now output:\n%s", out)
	}
	out, err = runScheduleCommand(t, gf, "runs", id)
	if err != nil {
		t.Fatalf("runs: %v", err)
	}
	if !strings.Contains(out, "fired") {
		t.Errorf("manual fire not recorded:\n%s", out)
	}
}

func scheduleRunRows(t *testing.T, db *sql.DB, keyPattern string) int {
	t.Helper()
	return countRows(t, db, `SELECT COUNT(*) FROM durable_run WHERE handler = ? AND state = 'running' AND key LIKE ?`, schedule.HandlerName, keyPattern)
}

func requireNoNote(t *testing.T, out string) {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "note:") {
			t.Fatalf("the command printed a note:\n%s", out)
		}
	}
}

func TestScheduleCommandsStartRunsForTheServer(t *testing.T) {
	t.Run("add", func(t *testing.T) {
		dataDir := t.TempDir()
		gf := &GlobalFlags{ConfigPath: writeScheduleCLIConfig(t, dataDir)}
		out, err := runScheduleCommand(t, gf, "add",
			"--name", "nightly", "--schedule", dailySchedule, "--timezone", "UTC",
			"--channel", "discord", "--destination", "default", "--prompt", "summarize")
		if err != nil {
			t.Fatalf("cron add: %v\n%s", err, out)
		}
		requireNoNote(t, out)
		db := openDatabaseFile(t, filepath.Join(dataDir, "aura.db"))
		var id string
		if err := db.QueryRowContext(t.Context(), `SELECT id FROM scheduled_job`).Scan(&id); err != nil {
			t.Fatalf("read the job: %v", err)
		}
		if n := scheduleRunRows(t, db, schedule.HandlerName+"/"+id+"/%"); n != 1 {
			t.Fatalf("schedule runs of the job = %d, want 1", n)
		}
	})
	t.Run("resume", func(t *testing.T) {
		dataDir := t.TempDir()
		gf := &GlobalFlags{ConfigPath: writeScheduleCLIConfig(t, dataDir)}
		db := openDatabaseFile(t, filepath.Join(dataDir, "aura.db"))
		jobs, err := schedule.NewSQLiteStore(db)
		if err != nil {
			t.Fatalf("NewSQLiteStore: %v", err)
		}
		service, err := schedule.NewService(jobs)
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		job, err := service.Create(t.Context(), &schedule.JobSpec{
			Name: "nightly", CronExpression: dailySchedule, Timezone: "UTC", Prompt: "summarize",
			OriginChannel: "discord", OriginDestination: "default", OverlapPolicy: schedule.OverlapSkip,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := service.SetState(t.Context(), job.ID, schedule.JobPaused); err != nil {
			t.Fatalf("SetState: %v", err)
		}
		pattern := schedule.HandlerName + "/" + job.ID + "/%"
		if n := scheduleRunRows(t, db, pattern); n != 0 {
			t.Fatalf("schedule runs before resume = %d, want 0", n)
		}
		out, err := runScheduleCommand(t, gf, "resume", job.ID)
		if err != nil {
			t.Fatalf("cron resume: %v\n%s", err, out)
		}
		requireNoNote(t, out)
		if n := scheduleRunRows(t, db, pattern); n != 1 {
			t.Fatalf("schedule runs after resume = %d, want 1", n)
		}
	})
	t.Run("run-now", func(t *testing.T) {
		dataDir := t.TempDir()
		gf := &GlobalFlags{ConfigPath: writeScheduleCLIConfig(t, dataDir)}
		id := addScheduleJob(t, gf)
		out, err := runScheduleCommand(t, gf, "run-now", id)
		if err != nil {
			t.Fatalf("cron run-now: %v\n%s", err, out)
		}
		requireNoNote(t, out)
		db := openDatabaseFile(t, filepath.Join(dataDir, "aura.db"))
		jobs, err := schedule.NewSQLiteStore(db)
		if err != nil {
			t.Fatalf("NewSQLiteStore: %v", err)
		}
		occurrences, err := jobs.Occurrences(t.Context(), id, 10)
		if err != nil || len(occurrences) != 1 {
			t.Fatalf("occurrences = %v, %v; want one", occurrences, err)
		}
		if n := scheduleRunRows(t, db, scheduleRunKey(id, occurrences[0].ScheduledForUTC)); n != 1 {
			t.Fatalf("schedule runs for the manual fire = %d, want 1", n)
		}
	})
}

func TestOperationCommandOutput(t *testing.T) {
	dataDir := t.TempDir()
	gf := &GlobalFlags{ConfigPath: writeScheduleCLIConfig(t, dataDir)}
	id := addScheduleJob(t, gf)

	t.Run("a record is key value lines", func(t *testing.T) {
		out, err := runScheduleCommand(t, gf, "pause", id)
		if err != nil || out != "id: "+id+"\nstate: paused\n" {
			t.Fatalf("pause output = %q, err = %v", out, err)
		}
	})
	t.Run("json prints the data object", func(t *testing.T) {
		out, err := runScheduleCommand(t, gf, "delete", id, "--json")
		want := "{\n  \"id\": \"" + id + "\",\n  \"state\": \"deleted\"\n}\n"
		if err != nil || out != want {
			t.Fatalf("delete --json output = %q, err = %v, want %q", out, err, want)
		}
	})
	t.Run("a missing required flag exits 2", func(t *testing.T) {
		_, err := runScheduleCommand(t, gf, "add", "--name", "x")
		if code := ExitCode(nil, nil, err); code != 2 {
			t.Fatalf("exit code = %d (%v), want 2", code, err)
		}
	})
	t.Run("a missing job id exits 2", func(t *testing.T) {
		_, err := runScheduleCommand(t, gf, "pause")
		if code := ExitCode(nil, nil, err); code != 2 {
			t.Fatalf("exit code = %d (%v), want 2", code, err)
		}
	})
	t.Run("a runs limit out of range exits 2", func(t *testing.T) {
		_, err := runScheduleCommand(t, gf, "runs", id, "--limit", "0")
		if code := ExitCode(nil, nil, err); code != 2 {
			t.Fatalf("exit code = %d (%v), want 2", code, err)
		}
	})
	t.Run("an unknown job exits 1", func(t *testing.T) {
		_, err := runScheduleCommand(t, gf, "pause", "missing")
		if code := ExitCode(nil, nil, err); code != 1 {
			t.Fatalf("exit code = %d (%v), want 1", code, err)
		}
	})
}

func TestOperationCommandOutputCronListGolden(t *testing.T) {
	dataDir := t.TempDir()
	gf := &GlobalFlags{ConfigPath: writeScheduleCLIConfig(t, dataDir)}
	db := openDatabaseFile(t, filepath.Join(dataDir, "aura.db"))
	jobs, err := schedule.NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	created := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for _, job := range []*schedule.Job{
		{ID: "job_0000000000000001", Name: "nightly", CronExpression: "0 21 * * *", Timezone: "UTC", Prompt: "summarize"},
		{ID: "job_0000000000000002", Name: "weekly report", CronExpression: "30 8 * * 1", Timezone: "Asia/Jakarta", Prompt: "report"},
	} {
		job.OriginChannel, job.OriginDestination = "discord", "default"
		job.OverlapPolicy, job.State, job.Version = schedule.OverlapSkip, schedule.JobPaused, 1
		job.CreatedAt, job.UpdatedAt = created, created
		if err := jobs.InsertJob(t.Context(), job); err != nil {
			t.Fatalf("InsertJob: %v", err)
		}
	}
	out, err := runScheduleCommand(t, gf, "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	compareAppGolden(t, "cron-list.txt", out)
}

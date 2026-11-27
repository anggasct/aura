package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/health"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/storage"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func fixedNow() time.Time { return time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC) }

func TestMigrationCheckerAgainstRealStore(t *testing.T) {
	ctx := context.Background()
	migrations, err := sqlite.Baseline(turnstore.Schema(), storage.Schema())
	if err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	db := sqlitetest.OpenMigrations(t, migrations)

	checker := health.MigrationChecker{Versions: func(ctx context.Context) (int, int, error) {
		return sqlite.SchemaVersions(ctx, db, migrations)
	}}
	findings := checker.Check(ctx)
	if len(findings) != 1 || findings[0].Status != health.StatusUp {
		t.Fatalf("migrated database findings = %+v, want a single up finding", findings)
	}
}

func TestMigrationCheckerBranches(t *testing.T) {
	cases := map[string]struct {
		applied, latest int
		err             error
		want            health.Status
		wantCode        string
	}{
		"current":   {applied: 1, latest: 1, want: health.StatusUp, wantCode: "ok"},
		"pending":   {applied: 0, latest: 1, want: health.StatusDegraded, wantCode: "migration_pending"},
		"downgrade": {applied: 2, latest: 1, want: health.StatusDown, wantCode: "migration_downgrade"},
		"unknown":   {err: errors.New("boom"), want: health.StatusUnknown, wantCode: "migration_unknown"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			checker := health.MigrationChecker{Versions: func(context.Context) (int, int, error) {
				return tc.applied, tc.latest, tc.err
			}}
			findings := checker.Check(context.Background())
			if len(findings) != 1 {
				t.Fatalf("findings = %+v, want exactly one", findings)
			}
			if findings[0].Status != tc.want || findings[0].Code != tc.wantCode {
				t.Errorf("finding = %+v, want status %s code %s", findings[0], tc.want, tc.wantCode)
			}
		})
	}
}

func TestCapabilityChecker(t *testing.T) {
	unavailable := health.CapabilityChecker{Statuses: func(context.Context) []health.CapabilityStatus {
		return []health.CapabilityStatus{
			{Name: "sandbox", Compiled: true, Enabled: true, MissingDependency: "cgroup_v2 delegation"},
			{Name: "model", Compiled: true, Available: true, Enabled: true},
		}
	}}
	findings := unavailable.Check(context.Background())
	if len(findings) != 1 || findings[0].Status != health.StatusDown || findings[0].Component != "capability/sandbox" {
		t.Fatalf("findings = %+v, want one down finding for capability/sandbox", findings)
	}
	if !strings.Contains(findings[0].Detail, "cgroup_v2") {
		t.Errorf("detail = %q, want the missing dependency reason", findings[0].Detail)
	}

	healthy := health.CapabilityChecker{Statuses: func(context.Context) []health.CapabilityStatus {
		return []health.CapabilityStatus{{Name: "model", Compiled: true, Available: true, Enabled: true}}
	}}
	if findings := healthy.Check(context.Background()); len(findings) != 1 || findings[0].Status != health.StatusUp {
		t.Fatalf("healthy findings = %+v, want a single up finding", findings)
	}

	notCompiled := health.CapabilityChecker{Statuses: func(context.Context) []health.CapabilityStatus {
		return []health.CapabilityStatus{{Name: "exec-linux", Compiled: false, Available: false, Enabled: true}}
	}}
	findings = notCompiled.Check(context.Background())
	if len(findings) != 1 || findings[0].Code != "capability_not_compiled" || findings[0].Status != health.StatusDown {
		t.Fatalf("not-compiled findings = %+v, want down capability_not_compiled", findings)
	}
}

func TestEffectJobChecker(t *testing.T) {
	stuck := health.EffectJobChecker{Stuck: func(context.Context) (int, error) { return 3, nil }}
	if f := stuck.Check(context.Background()); f[0].Status != health.StatusDegraded || f[0].Code != "effect_job_stuck" {
		t.Errorf("stuck finding = %+v, want degraded effect_job_stuck", f[0])
	}

	none := health.EffectJobChecker{Stuck: func(context.Context) (int, error) { return 0, nil }}
	if f := none.Check(context.Background()); f[0].Status != health.StatusUp {
		t.Errorf("clear finding = %+v, want up", f[0])
	}

	unknown := health.EffectJobChecker{Stuck: func(context.Context) (int, error) { return 0, errors.New("boom") }}
	if f := unknown.Check(context.Background()); f[0].Status != health.StatusUnknown || f[0].Code != "effect_job_unknown" {
		t.Errorf("unknown finding = %+v, want unknown effect_job_unknown", f[0])
	}
}

func TestSandboxChecker(t *testing.T) {
	up := health.SandboxChecker{Support: func() (bool, string) { return true, "seccomp landlock" }}
	if f := up.Check(context.Background()); f[0].Status != health.StatusUp {
		t.Errorf("supported sandbox status = %s, want up", f[0].Status)
	}
	down := health.SandboxChecker{Support: func() (bool, string) { return false, "no primitives" }}
	if f := down.Check(context.Background()); f[0].Status != health.StatusDown || f[0].Code != "sandbox_unavailable" {
		t.Errorf("unsupported sandbox finding = %+v, want down sandbox_unavailable", f[0])
	}
}

func TestBackupCheckerAgainstRealManifest(t *testing.T) {
	dir := t.TempDir()
	manifest := storage.BackupManifest{CreatedAt: fixedNow().Add(-2 * time.Hour)}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	checker := health.BackupChecker{
		LastBackup: func(context.Context) (time.Time, error) {
			m, err := storage.ReadBackupManifest(dir)
			if err != nil {
				return time.Time{}, err
			}
			return m.CreatedAt, nil
		},
		MaxAge: 24 * time.Hour,
		Now:    fixedNow,
	}
	if f := checker.Check(context.Background()); f[0].Status != health.StatusUp {
		t.Fatalf("fresh backup finding = %+v, want up", f[0])
	}

	stale := checker
	stale.MaxAge = time.Hour
	if f := stale.Check(context.Background()); f[0].Status != health.StatusDegraded || f[0].Code != "backup_stale" {
		t.Fatalf("stale backup finding = %+v, want degraded backup_stale", f[0])
	}

	missing := health.BackupChecker{
		LastBackup: func(context.Context) (time.Time, error) { return time.Time{}, errors.New("no backup") },
		MaxAge:     24 * time.Hour,
		Now:        fixedNow,
	}
	if f := missing.Check(context.Background()); f[0].Status != health.StatusDown || f[0].Code != "backup_missing" {
		t.Fatalf("missing backup finding = %+v, want down backup_missing", f[0])
	}
}

func TestProviderChecker(t *testing.T) {
	configured := health.ProviderChecker{Probe: func(context.Context) error { return nil }}
	if f := configured.Check(context.Background()); f[0].Status != health.StatusUp {
		t.Errorf("configured provider status = %s, want up", f[0].Status)
	}
	missing := health.ProviderChecker{Probe: func(context.Context) error { return errors.New("not configured") }}
	if f := missing.Check(context.Background()); f[0].Status != health.StatusDown || f[0].Code != "provider_unavailable" {
		t.Errorf("missing provider finding = %+v, want down provider_unavailable", f[0])
	}
}

func TestEvaluatorReportsWorstStatus(t *testing.T) {
	eval := health.NewEvaluator(
		health.MigrationChecker{Versions: func(context.Context) (int, int, error) { return 1, 1, nil }},
		health.BackupChecker{LastBackup: func(context.Context) (time.Time, error) { return time.Time{}, errors.New("none") }, MaxAge: time.Hour, Now: fixedNow},
		health.ProviderChecker{Probe: func(context.Context) error { return nil }},
	)
	if got := eval.Status(context.Background()); got != health.StatusDown {
		t.Errorf("evaluator status = %s, want down (worst of up/down/up)", got)
	}
	findings := eval.Evaluate(context.Background())
	if len(findings) != 3 {
		t.Errorf("findings = %d, want 3 (one per checker)", len(findings))
	}
}

func TestEvaluatorEmptyIsUp(t *testing.T) {
	if got := health.NewEvaluator().Status(context.Background()); got != health.StatusUp {
		t.Errorf("empty evaluator status = %s, want up", got)
	}
}

func TestEventLogAndStorageIntakeProbeNilGuards(t *testing.T) {
	var nilLog *health.EventLog
	if err := nilLog.Sink(t.Context(), &health.Transition{}); err == nil {
		t.Fatal("nil EventLog.Sink = nil, want error")
	}
	if _, err := nilLog.History(t.Context()); err == nil {
		t.Fatal("nil EventLog.History = nil, want error")
	}
	emptyLog := health.NewEventLog(nil, nil)
	if err := emptyLog.Sink(t.Context(), nil); err == nil {
		t.Fatal("empty EventLog.Sink = nil, want error")
	}
	if _, err := emptyLog.History(t.Context()); err == nil {
		t.Fatal("empty EventLog.History = nil, want error")
	}
	probe := health.StorageIntakeProbe("/nonexistent", nil, nil, nil)
	if state, _ := probe(t.Context()); state != health.StorageIntakeUnknown {
		t.Fatalf("StorageIntakeProbe with nil deps = %s, want %s", state, health.StorageIntakeUnknown)
	}
}

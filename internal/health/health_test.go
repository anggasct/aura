package health

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/store"
)

func fixedNow() time.Time { return time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC) }

func TestMigrationCheckerAgainstRealStore(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenDB(ctx, filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	checker := MigrationChecker{Versions: func(ctx context.Context) (int, int, error) {
		return store.SchemaVersions(ctx, db)
	}}
	findings := checker.Check(ctx)
	if len(findings) != 1 || findings[0].Status != StatusUp {
		t.Fatalf("migrated database findings = %+v, want a single up finding", findings)
	}
}

func TestMigrationCheckerBranches(t *testing.T) {
	cases := map[string]struct {
		applied, latest int
		err             error
		want            Status
		wantCode        string
	}{
		"current":   {applied: 1, latest: 1, want: StatusUp, wantCode: "ok"},
		"pending":   {applied: 0, latest: 1, want: StatusDegraded, wantCode: "migration_pending"},
		"downgrade": {applied: 2, latest: 1, want: StatusDown, wantCode: "migration_downgrade"},
		"unknown":   {err: errors.New("boom"), want: StatusUnknown, wantCode: "migration_unknown"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			checker := MigrationChecker{Versions: func(context.Context) (int, int, error) {
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
	unavailable := CapabilityChecker{Statuses: func(context.Context) []CapabilityStatus {
		return []CapabilityStatus{{Name: "sandbox", Available: false}, {Name: "model", Available: true}}
	}}
	findings := unavailable.Check(context.Background())
	if len(findings) != 1 || findings[0].Status != StatusDown || findings[0].Component != "capability/sandbox" {
		t.Fatalf("findings = %+v, want one down finding for capability/sandbox", findings)
	}

	healthy := CapabilityChecker{Statuses: func(context.Context) []CapabilityStatus {
		return []CapabilityStatus{{Name: "model", Available: true}}
	}}
	if findings := healthy.Check(context.Background()); len(findings) != 1 || findings[0].Status != StatusUp {
		t.Fatalf("healthy findings = %+v, want a single up finding", findings)
	}
}

func TestEffectJobChecker(t *testing.T) {
	stuck := EffectJobChecker{Stuck: func(context.Context) (int, error) { return 3, nil }}
	if f := stuck.Check(context.Background()); f[0].Status != StatusDegraded || f[0].Code != "effect_job_stuck" {
		t.Errorf("stuck finding = %+v, want degraded effect_job_stuck", f[0])
	}

	none := EffectJobChecker{Stuck: func(context.Context) (int, error) { return 0, nil }}
	if f := none.Check(context.Background()); f[0].Status != StatusUp {
		t.Errorf("clear finding = %+v, want up", f[0])
	}

	unknown := EffectJobChecker{Stuck: func(context.Context) (int, error) { return 0, errors.New("boom") }}
	if f := unknown.Check(context.Background()); f[0].Status != StatusUnknown || f[0].Code != "effect_job_unknown" {
		t.Errorf("unknown finding = %+v, want unknown effect_job_unknown", f[0])
	}
}

func TestSandboxChecker(t *testing.T) {
	up := SandboxChecker{Support: func() (bool, string) { return true, "seccomp landlock" }}
	if f := up.Check(context.Background()); f[0].Status != StatusUp {
		t.Errorf("supported sandbox status = %s, want up", f[0].Status)
	}
	down := SandboxChecker{Support: func() (bool, string) { return false, "no primitives" }}
	// Mandatory containment is absent: the finding is down so readiness and
	// exit codes both block, never a benign degraded line.
	if f := down.Check(context.Background()); f[0].Status != StatusDown || f[0].Code != "sandbox_unavailable" {
		t.Errorf("unsupported sandbox finding = %+v, want down sandbox_unavailable", f[0])
	}
}

func TestBackupCheckerAgainstRealManifest(t *testing.T) {
	dir := t.TempDir()
	manifest := store.BackupManifest{CreatedAt: fixedNow().Add(-2 * time.Hour)}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	checker := BackupChecker{
		LastBackup: func(context.Context) (time.Time, error) {
			m, err := store.ReadBackupManifest(dir)
			if err != nil {
				return time.Time{}, err
			}
			return m.CreatedAt, nil
		},
		MaxAge: 24 * time.Hour,
		Now:    fixedNow,
	}
	if f := checker.Check(context.Background()); f[0].Status != StatusUp {
		t.Fatalf("fresh backup finding = %+v, want up", f[0])
	}

	stale := checker
	stale.MaxAge = time.Hour
	if f := stale.Check(context.Background()); f[0].Status != StatusDegraded || f[0].Code != "backup_stale" {
		t.Fatalf("stale backup finding = %+v, want degraded backup_stale", f[0])
	}

	missing := BackupChecker{
		LastBackup: func(context.Context) (time.Time, error) { return time.Time{}, errors.New("no backup") },
		MaxAge:     24 * time.Hour,
		Now:        fixedNow,
	}
	if f := missing.Check(context.Background()); f[0].Status != StatusDown || f[0].Code != "backup_missing" {
		t.Fatalf("missing backup finding = %+v, want down backup_missing", f[0])
	}
}

func TestProviderChecker(t *testing.T) {
	configured := ProviderChecker{Probe: func(context.Context) error { return nil }}
	if f := configured.Check(context.Background()); f[0].Status != StatusUp {
		t.Errorf("configured provider status = %s, want up", f[0].Status)
	}
	missing := ProviderChecker{Probe: func(context.Context) error { return errors.New("not configured") }}
	if f := missing.Check(context.Background()); f[0].Status != StatusDown || f[0].Code != "provider_unavailable" {
		t.Errorf("missing provider finding = %+v, want down provider_unavailable", f[0])
	}
}

func TestEvaluatorReportsWorstStatus(t *testing.T) {
	eval := NewEvaluator(
		MigrationChecker{Versions: func(context.Context) (int, int, error) { return 1, 1, nil }},
		BackupChecker{LastBackup: func(context.Context) (time.Time, error) { return time.Time{}, errors.New("none") }, MaxAge: time.Hour, Now: fixedNow},
		ProviderChecker{Probe: func(context.Context) error { return nil }},
	)
	if got := eval.Status(context.Background()); got != StatusDown {
		t.Errorf("evaluator status = %s, want down (worst of up/down/up)", got)
	}
	findings := eval.Evaluate(context.Background())
	if len(findings) != 3 {
		t.Errorf("findings = %d, want 3 (one per checker)", len(findings))
	}
}

func TestEvaluatorEmptyIsUp(t *testing.T) {
	if got := NewEvaluator().Status(context.Background()); got != StatusUp {
		t.Errorf("empty evaluator status = %s, want up", got)
	}
}

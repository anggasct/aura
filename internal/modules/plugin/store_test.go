package plugin

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/store/history"
	"github.com/anggasct/aura/internal/store/storetest"
)

var pluginTestTime = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

func newTestSQLiteStore(t *testing.T) (*SQLiteStore, *sql.DB) {
	t.Helper()
	db := storetest.OpenMigrated(t)
	s, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	return s, db
}

func testInstallRecord(id, version, state, approved string) *InstallRecord {
	return &InstallRecord{
		Plugin:  Record{ID: id, State: state, Version: version, ApprovedVersion: approved, UpdatedAt: pluginTestTime},
		Version: VersionRecord{ID: id, Version: version, Digest: "digest-" + version, ManifestJSON: `{"id":"` + id + `"}`, InstalledAt: pluginTestTime},
	}
}

func testGrantRecord(id, surface, value string) GrantRecord {
	return GrantRecord{
		ID:          id,
		GrantKey:    GrantKey{Surface: surface, Value: value},
		Fingerprint: "fp-" + value,
		ApprovedAt:  pluginTestTime,
		ApprovedBy:  ApprovedByOwner,
	}
}

func TestNewSQLiteStoreRejectsNilDB(t *testing.T) {
	_, err := NewSQLiteStore(nil)
	if !fault.HasCode(err, ErrInvalidArgument) {
		t.Fatalf("NewSQLiteStore(nil) = %v, want %s", err, ErrInvalidArgument)
	}
}

func TestPluginMigrationV37Schema(t *testing.T) {
	db := storetest.OpenMigrated(t)
	applied, latest, err := sqlite.SchemaVersions(t.Context(), db, history.Migrations())
	if err != nil || applied != latest || applied < 48 {
		t.Fatalf("schema = applied %d latest %d %v, want latest >= 48", applied, latest, err)
	}
	for _, table := range []string{"plugin", "plugin_version", "plugin_grant"} {
		var name string
		if err := db.QueryRowContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

func TestPluginInstallRoundTrip(t *testing.T) {
	ctx := t.Context()
	plugins, _ := newTestSQLiteStore(t)
	in := testInstallRecord("weather", "1.0.0", StateQuarantined, "")
	if err := plugins.Install(ctx, in); err != nil {
		t.Fatalf("Install: %v", err)
	}
	got, err := plugins.Get(ctx, "weather")
	if err != nil || got != in.Plugin {
		t.Fatalf("Get = %+v, %v; want %+v", got, err, in.Plugin)
	}
	version, err := plugins.Version(ctx, "weather", "1.0.0")
	if err != nil || version != in.Version {
		t.Fatalf("Version = %+v, %v; want %+v", version, err, in.Version)
	}
	listed, err := plugins.List(ctx)
	if err != nil || len(listed) != 1 || listed[0].ID != "weather" {
		t.Fatalf("List = %v, %v", listed, err)
	}
}

func TestPluginInstallValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *InstallRecord)
		want   ErrorCode
	}{
		{"bad state", func(in *InstallRecord) { in.Plugin.State = "bogus" }, ErrInvalidArgument},
		{"empty id", func(in *InstallRecord) { in.Plugin.ID = "" }, ErrInvalidArgument},
		{"version rows disagree", func(in *InstallRecord) { in.Version.Version = "2.0.0" }, ErrInvalidArgument},
		{"missing digest", func(in *InstallRecord) { in.Version.Digest = "" }, ErrInvalidArgument},
		{"missing time", func(in *InstallRecord) { in.Plugin.UpdatedAt = time.Time{} }, ErrInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := testInstallRecord("weather", "1.0.0", StateQuarantined, "")
			tc.mutate(in)
			plugins, _ := newTestSQLiteStore(t)
			err := plugins.Install(t.Context(), in)
			if !fault.HasCode(err, tc.want) {
				t.Fatalf("Install error = %v, want %s", err, tc.want)
			}
		})
	}
	plugins, _ := newTestSQLiteStore(t)
	if err := plugins.Install(t.Context(), nil); !fault.HasCode(err, ErrInvalidArgument) {
		t.Fatalf("Install(nil) = %v, want %s", err, ErrInvalidArgument)
	}
}

func TestPluginInstallDuplicateVersionConflictsAndRollsBack(t *testing.T) {
	ctx := t.Context()
	plugins, _ := newTestSQLiteStore(t)
	if err := plugins.Install(ctx, testInstallRecord("weather", "1.0.0", StateQuarantined, "")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	again := testInstallRecord("weather", "1.0.0", StateActive, "1.0.0")
	err := plugins.Install(ctx, again)
	if !fault.HasCode(err, ErrVersionConflict) {
		t.Fatalf("Install duplicate error = %v, want %s", err, ErrVersionConflict)
	}
	got, err := plugins.Get(ctx, "weather")
	if err != nil || got.State != StateQuarantined || got.ApprovedVersion != "" {
		t.Fatalf("plugin after rolled back install = %+v, %v", got, err)
	}
}

func TestPluginStoreJoinsCallersTransaction(t *testing.T) {
	ctx := t.Context()
	plugins, db := newTestSQLiteStore(t)
	sentinel := errors.New("abort caller transaction")
	err := sqlite.RunInTx(ctx, db, func(txCtx context.Context) error {
		if err := plugins.Install(txCtx, testInstallRecord("weather", "1.0.0", StateQuarantined, "")); err != nil {
			return err
		}
		got, err := plugins.Get(txCtx, "weather")
		if err != nil || got.ID != "weather" {
			t.Fatalf("Get inside tx = %+v, %v", got, err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunInTx error = %v, want %v", err, sentinel)
	}
	if _, err := plugins.Get(ctx, "weather"); !fault.HasCode(err, ErrNotFound) {
		t.Fatalf("Get after rollback = %v, want %s", err, ErrNotFound)
	}
}

func TestPluginApproveReplacesGrantsAndActivates(t *testing.T) {
	ctx := t.Context()
	plugins, _ := newTestSQLiteStore(t)
	if err := plugins.Install(ctx, testInstallRecord("weather", "1.0.0", StateQuarantined, "")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	approve := func(grants ...GrantRecord) error {
		return plugins.Approve(ctx, &ApprovalRecord{ID: "weather", Version: "1.0.0", Digest: "digest-1.0.0", Grants: grants, At: pluginTestTime.Add(time.Minute)})
	}
	if err := approve(testGrantRecord("weather", SurfaceTool, "forecast"), testGrantRecord("weather", SurfaceEgress, "api.example.com:443")); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	got, err := plugins.Get(ctx, "weather")
	if err != nil || got.State != StateActive || got.ApprovedVersion != "1.0.0" {
		t.Fatalf("plugin after approve = %+v, %v", got, err)
	}
	if err := approve(testGrantRecord("weather", SurfaceTool, "radar")); err != nil {
		t.Fatalf("second Approve: %v", err)
	}
	grants, err := plugins.Grants(ctx, "weather")
	if err != nil || len(grants) != 1 || grants[0].Value != "radar" {
		t.Fatalf("Grants after replace = %+v, %v", grants, err)
	}
}

func TestPluginApproveRejectsStaleOrUnknown(t *testing.T) {
	cases := []struct {
		name     string
		approval ApprovalRecord
		want     ErrorCode
	}{
		{"digest differs", ApprovalRecord{ID: "weather", Version: "1.0.0", Digest: "other", At: pluginTestTime}, ErrDigestMismatch},
		{"version is not current", ApprovalRecord{ID: "weather", Version: "0.9.0", Digest: "digest-1.0.0", At: pluginTestTime}, ErrDigestMismatch},
		{"unknown plugin", ApprovalRecord{ID: "missing", Version: "1.0.0", Digest: "digest-1.0.0", At: pluginTestTime}, ErrNotFound},
		{"grant for another plugin", ApprovalRecord{ID: "weather", Version: "1.0.0", Digest: "digest-1.0.0", At: pluginTestTime, Grants: []GrantRecord{testGrantRecord("other", SurfaceTool, "x")}}, ErrInvalidArgument},
		{"grant surface unknown", ApprovalRecord{ID: "weather", Version: "1.0.0", Digest: "digest-1.0.0", At: pluginTestTime, Grants: []GrantRecord{testGrantRecord("weather", "bogus", "x")}}, ErrInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			plugins, _ := newTestSQLiteStore(t)
			if err := plugins.Install(ctx, testInstallRecord("weather", "1.0.0", StateQuarantined, "")); err != nil {
				t.Fatalf("Install: %v", err)
			}
			err := plugins.Approve(ctx, &tc.approval)
			if !fault.HasCode(err, tc.want) {
				t.Fatalf("Approve error = %v, want %s", err, tc.want)
			}
			got, getErr := plugins.Get(ctx, "weather")
			if getErr != nil || got.State != StateQuarantined {
				t.Fatalf("plugin after failed approve = %+v, %v", got, getErr)
			}
		})
	}
}

func TestPluginInstallDropsGrantsAndKeepsApprovedVersion(t *testing.T) {
	ctx := t.Context()
	plugins, _ := newTestSQLiteStore(t)
	if err := plugins.Install(ctx, testInstallRecord("weather", "1.0.0", StateQuarantined, "")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := plugins.Approve(ctx, &ApprovalRecord{ID: "weather", Version: "1.0.0", Digest: "digest-1.0.0", At: pluginTestTime, Grants: []GrantRecord{
		testGrantRecord("weather", SurfaceTool, "forecast"), testGrantRecord("weather", SurfaceTool, "radar"),
	}}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	update := testInstallRecord("weather", "1.1.0", StateActive, "1.1.0")
	update.DropGrants = []GrantKey{{Surface: SurfaceTool, Value: "radar"}}
	if err := plugins.Install(ctx, update); err != nil {
		t.Fatalf("Install update: %v", err)
	}
	grants, err := plugins.Grants(ctx, "weather")
	if err != nil || len(grants) != 1 || grants[0].Value != "forecast" {
		t.Fatalf("Grants after drop = %+v, %v", grants, err)
	}
	got, err := plugins.Get(ctx, "weather")
	if err != nil || got.Version != "1.1.0" || got.ApprovedVersion != "1.1.0" {
		t.Fatalf("plugin after update = %+v, %v", got, err)
	}
}

func TestPluginSetState(t *testing.T) {
	ctx := t.Context()
	plugins, _ := newTestSQLiteStore(t)
	if err := plugins.Install(ctx, testInstallRecord("weather", "1.0.0", StateActive, "1.0.0")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := plugins.SetState(ctx, "weather", StateDisabled, pluginTestTime.Add(time.Hour)); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	got, err := plugins.Get(ctx, "weather")
	if err != nil || got.State != StateDisabled {
		t.Fatalf("plugin after SetState = %+v, %v", got, err)
	}
	if err := plugins.SetState(ctx, "weather", "bogus", pluginTestTime); err == nil {
		t.Fatal("SetState accepted an unknown state")
	}
	if err := plugins.SetState(ctx, "missing", StateDisabled, pluginTestTime); !fault.HasCode(err, ErrNotFound) {
		t.Fatalf("SetState unknown plugin error = %v, want %s", err, ErrNotFound)
	}
}

func TestPluginRemoveCascades(t *testing.T) {
	ctx := t.Context()
	plugins, db := newTestSQLiteStore(t)
	if err := plugins.Install(ctx, testInstallRecord("weather", "1.0.0", StateQuarantined, "")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := plugins.Approve(ctx, &ApprovalRecord{ID: "weather", Version: "1.0.0", Digest: "digest-1.0.0", At: pluginTestTime, Grants: []GrantRecord{testGrantRecord("weather", SurfaceTool, "forecast")}}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := plugins.Remove(ctx, "weather"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, table := range []string{"plugin", "plugin_version", "plugin_grant"} {
		var count int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Errorf("table %s rows = %d, %v; want 0", table, count, err)
		}
	}
	if err := plugins.Remove(ctx, "weather"); !fault.HasCode(err, ErrNotFound) {
		t.Fatalf("second Remove error = %v, want %s", err, ErrNotFound)
	}
}

func TestPluginReadsReportNotFound(t *testing.T) {
	plugins, _ := newTestSQLiteStore(t)
	if _, err := plugins.Get(t.Context(), "missing"); !fault.HasCode(err, ErrNotFound) {
		t.Errorf("Get error = %v", err)
	}
	if _, err := plugins.Version(t.Context(), "missing", "1.0.0"); !fault.HasCode(err, ErrNotFound) {
		t.Errorf("Version error = %v", err)
	}
}

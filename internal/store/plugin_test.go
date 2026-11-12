package store

import (
	"testing"
	"time"
)

var pluginTestTime = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

func pluginInstall(id, version, state, approved string) *PluginInstall {
	return &PluginInstall{
		Plugin:  Plugin{ID: id, State: state, Version: version, ApprovedVersion: approved, UpdatedAt: pluginTestTime},
		Version: PluginVersion{PluginID: id, Version: version, Digest: "digest-" + version, ManifestJSON: `{"id":"` + id + `"}`, InstalledAt: pluginTestTime},
	}
}

func pluginGrant(id, surface, value string) PluginGrant {
	return PluginGrant{PluginID: id, Surface: surface, Value: value, Fingerprint: "fp-" + value, ApprovedAt: pluginTestTime, ApprovedBy: "owner"}
}

func TestPluginMigrationV37Schema(t *testing.T) {
	db := newTestDB(t)
	applied, latest, err := SchemaVersions(t.Context(), db)
	if err != nil || applied != 44 || latest != 44 {
		t.Fatalf("schema = applied %d latest %d %v, want 44", applied, latest, err)
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
	plugins := NewPluginStore(newTestDB(t))
	in := pluginInstall("weather", "1.0.0", PluginStateQuarantined, "")
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
		mutate func(in *PluginInstall)
		want   ErrorCode
	}{
		{"bad state", func(in *PluginInstall) { in.Plugin.State = "bogus" }, ErrorCodePluginInvalid},
		{"empty id", func(in *PluginInstall) { in.Plugin.ID = "" }, ErrorCodePluginInvalid},
		{"version rows disagree", func(in *PluginInstall) { in.Version.Version = "2.0.0" }, ErrorCodePluginInvalid},
		{"missing digest", func(in *PluginInstall) { in.Version.Digest = "" }, ErrorCodePluginInvalid},
		{"missing time", func(in *PluginInstall) { in.Plugin.UpdatedAt = time.Time{} }, ErrorCodePluginInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := pluginInstall("weather", "1.0.0", PluginStateQuarantined, "")
			tc.mutate(in)
			err := NewPluginStore(newTestDB(t)).Install(t.Context(), in)
			if code, ok := CodeOf(err); !ok || code != tc.want {
				t.Fatalf("Install error = %v, want %s", err, tc.want)
			}
		})
	}
	if err := NewPluginStore(newTestDB(t)).Install(t.Context(), nil); err == nil {
		t.Fatal("Install(nil) succeeded")
	}
}

func TestPluginInstallDuplicateVersionConflictsAndRollsBack(t *testing.T) {
	ctx := t.Context()
	plugins := NewPluginStore(newTestDB(t))
	if err := plugins.Install(ctx, pluginInstall("weather", "1.0.0", PluginStateQuarantined, "")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	again := pluginInstall("weather", "1.0.0", PluginStateActive, "1.0.0")
	err := plugins.Install(ctx, again)
	if code, ok := CodeOf(err); !ok || code != ErrorCodePluginConflict {
		t.Fatalf("Install duplicate error = %v, want %s", err, ErrorCodePluginConflict)
	}
	got, err := plugins.Get(ctx, "weather")
	if err != nil || got.State != PluginStateQuarantined || got.ApprovedVersion != "" {
		t.Fatalf("plugin after rolled back install = %+v, %v", got, err)
	}
}

func TestPluginApproveReplacesGrantsAndActivates(t *testing.T) {
	ctx := t.Context()
	plugins := NewPluginStore(newTestDB(t))
	if err := plugins.Install(ctx, pluginInstall("weather", "1.0.0", PluginStateQuarantined, "")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	approve := func(grants ...PluginGrant) error {
		return plugins.Approve(ctx, &PluginApproval{PluginID: "weather", Version: "1.0.0", Digest: "digest-1.0.0", Grants: grants, At: pluginTestTime.Add(time.Minute)})
	}
	if err := approve(pluginGrant("weather", PluginSurfaceTool, "forecast"), pluginGrant("weather", PluginSurfaceEgress, "api.example.com:443")); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	got, err := plugins.Get(ctx, "weather")
	if err != nil || got.State != PluginStateActive || got.ApprovedVersion != "1.0.0" {
		t.Fatalf("plugin after approve = %+v, %v", got, err)
	}
	if err := approve(pluginGrant("weather", PluginSurfaceTool, "radar")); err != nil {
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
		approval PluginApproval
		want     ErrorCode
	}{
		{"digest differs", PluginApproval{PluginID: "weather", Version: "1.0.0", Digest: "other", At: pluginTestTime}, ErrorCodePluginConflict},
		{"version is not current", PluginApproval{PluginID: "weather", Version: "0.9.0", Digest: "digest-1.0.0", At: pluginTestTime}, ErrorCodePluginConflict},
		{"unknown plugin", PluginApproval{PluginID: "missing", Version: "1.0.0", Digest: "digest-1.0.0", At: pluginTestTime}, ErrorCodePluginNotFound},
		{"grant for another plugin", PluginApproval{PluginID: "weather", Version: "1.0.0", Digest: "digest-1.0.0", At: pluginTestTime, Grants: []PluginGrant{pluginGrant("other", PluginSurfaceTool, "x")}}, ErrorCodePluginInvalid},
		{"grant surface unknown", PluginApproval{PluginID: "weather", Version: "1.0.0", Digest: "digest-1.0.0", At: pluginTestTime, Grants: []PluginGrant{pluginGrant("weather", "bogus", "x")}}, ErrorCodePluginInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			plugins := NewPluginStore(newTestDB(t))
			if err := plugins.Install(ctx, pluginInstall("weather", "1.0.0", PluginStateQuarantined, "")); err != nil {
				t.Fatalf("Install: %v", err)
			}
			err := plugins.Approve(ctx, &tc.approval)
			if code, ok := CodeOf(err); !ok || code != tc.want {
				t.Fatalf("Approve error = %v, want %s", err, tc.want)
			}
			got, getErr := plugins.Get(ctx, "weather")
			if getErr != nil || got.State != PluginStateQuarantined {
				t.Fatalf("plugin after failed approve = %+v, %v", got, getErr)
			}
		})
	}
}

func TestPluginInstallDropsGrantsAndKeepsApprovedVersion(t *testing.T) {
	ctx := t.Context()
	plugins := NewPluginStore(newTestDB(t))
	if err := plugins.Install(ctx, pluginInstall("weather", "1.0.0", PluginStateQuarantined, "")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := plugins.Approve(ctx, &PluginApproval{PluginID: "weather", Version: "1.0.0", Digest: "digest-1.0.0", At: pluginTestTime, Grants: []PluginGrant{
		pluginGrant("weather", PluginSurfaceTool, "forecast"), pluginGrant("weather", PluginSurfaceTool, "radar"),
	}}); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	update := pluginInstall("weather", "1.1.0", PluginStateActive, "1.1.0")
	update.DropGrants = []PluginGrantKey{{Surface: PluginSurfaceTool, Value: "radar"}}
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
	plugins := NewPluginStore(newTestDB(t))
	if err := plugins.Install(ctx, pluginInstall("weather", "1.0.0", PluginStateActive, "1.0.0")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := plugins.SetState(ctx, "weather", PluginStateDisabled, pluginTestTime.Add(time.Hour)); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	got, err := plugins.Get(ctx, "weather")
	if err != nil || got.State != PluginStateDisabled {
		t.Fatalf("plugin after SetState = %+v, %v", got, err)
	}
	if err := plugins.SetState(ctx, "weather", "bogus", pluginTestTime); err == nil {
		t.Fatal("SetState accepted an unknown state")
	}
	if code, ok := CodeOf(plugins.SetState(ctx, "missing", PluginStateDisabled, pluginTestTime)); !ok || code != ErrorCodePluginNotFound {
		t.Fatalf("SetState unknown plugin code = %v, %v", code, ok)
	}
}

func TestPluginRemoveCascades(t *testing.T) {
	ctx := t.Context()
	db := newTestDB(t)
	plugins := NewPluginStore(db)
	if err := plugins.Install(ctx, pluginInstall("weather", "1.0.0", PluginStateQuarantined, "")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := plugins.Approve(ctx, &PluginApproval{PluginID: "weather", Version: "1.0.0", Digest: "digest-1.0.0", At: pluginTestTime, Grants: []PluginGrant{pluginGrant("weather", PluginSurfaceTool, "forecast")}}); err != nil {
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
	if code, ok := CodeOf(plugins.Remove(ctx, "weather")); !ok || code != ErrorCodePluginNotFound {
		t.Fatalf("second Remove code = %v, %v", code, ok)
	}
}

func TestPluginReadsReportNotFound(t *testing.T) {
	plugins := NewPluginStore(newTestDB(t))
	if _, err := plugins.Get(t.Context(), "missing"); !hasStoreCode(err, ErrorCodePluginNotFound) {
		t.Errorf("Get error = %v", err)
	}
	if _, err := plugins.Version(t.Context(), "missing", "1.0.0"); !hasStoreCode(err, ErrorCodePluginNotFound) {
		t.Errorf("Version error = %v", err)
	}
}

func hasStoreCode(err error, want ErrorCode) bool {
	code, ok := CodeOf(err)
	return ok && code == want
}

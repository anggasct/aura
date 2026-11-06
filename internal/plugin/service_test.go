package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func withTool(t *testing.T, manifest, name, tier string) string {
	t.Helper()
	return replaceOnce(t, manifest, "egress:", "  - name: "+name+"\n    description: Tool "+name+".\n    tier: "+tier+"\negress:")
}

func bump(t *testing.T, manifest, version string) string {
	t.Helper()
	return replaceOnce(t, manifest, "version: 1.0.0", "version: "+version)
}

func TestApproveBindsToTheDigest(t *testing.T) {
	cases := []struct {
		name   string
		prefix func(digest string) string
		want   ErrorCode
	}{
		{"matching prefix", func(d string) string { return d[:12] }, ""},
		{"full digest", func(d string) string { return d }, ""},
		{"other digest", func(d string) string { return strings.Repeat("0", 12) }, ErrDigestMismatch},
		{"flipped last character of a long prefix", func(d string) string {
			if d[15] == 'a' {
				return d[:15] + "b"
			}
			return d[:15] + "a"
		}, ErrDigestMismatch},
		{"too short", func(d string) string { return d[:11] }, ErrInvalidArgument},
		{"empty", func(string) string { return "" }, ErrInvalidArgument},
		{"uppercase", func(d string) string { return strings.ToUpper(d[:12]) }, ErrInvalidArgument},
		{"not hex", func(string) string { return "zzzzzzzzzzzz" }, ErrInvalidArgument},
		{"longer than a digest", func(d string) string { return d + "0" }, ErrInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			result := env.install(baseManifest, serverFiles())
			_, err := env.service.Approve(t.Context(), "weather", tc.prefix(result.Digest))
			record, _ := env.registry.Get(t.Context(), "weather")
			if tc.want == "" {
				if err != nil || record.State != StateActive || record.ApprovedVersion != "1.0.0" {
					t.Fatalf("Approve = %v, record %+v", err, record)
				}
				return
			}
			if !hasCode(err, tc.want) || record.State != StateQuarantined {
				t.Fatalf("Approve error = %v, record %+v, want %s", err, record, tc.want)
			}
		})
	}
}

func TestApproveRecordsTheFullSurface(t *testing.T) {
	env := newTestEnv(t)
	result := env.install(baseManifest, serverFiles())
	approved, err := env.service.Approve(t.Context(), "weather", result.Digest[:12])
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if !slices.Equal(approved.Tools, []string{"weather.alerts", "weather.forecast"}) {
		t.Fatalf("approved tools = %v", approved.Tools)
	}
	grants, _ := env.registry.Grants(t.Context(), "weather")
	var keys []GrantKey
	for _, grant := range grants {
		keys = append(keys, grant.GrantKey)
		if grant.ApprovedBy != ApprovedByOwner || !grant.ApprovedAt.Equal(env.clock) {
			t.Errorf("grant %+v lacks its approval record", grant)
		}
	}
	want := []GrantKey{{SurfaceCapability, "network"}, {SurfaceEgress, "api.weather.example"}, {SurfaceTool, "alerts"}, {SurfaceTool, "forecast"}}
	if !slices.Equal(keys, want) {
		t.Fatalf("grants = %v", keys)
	}
}

func TestApproveFailsWhenTheBundleChangedAfterInstall(t *testing.T) {
	env := newTestEnv(t)
	result := env.install(baseManifest, serverFiles())
	if err := os.WriteFile(filepath.Join(env.dir, "weather", "1.0.0", "server"), []byte("#!/bin/sh\nevil\n"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := env.service.Approve(t.Context(), "weather", result.Digest[:12])
	if !hasCode(err, ErrTampered) {
		t.Fatalf("Approve error = %v, want %s", err, ErrTampered)
	}
	if record, _ := env.registry.Get(t.Context(), "weather"); record.State != StateQuarantined {
		t.Fatalf("state = %s", record.State)
	}
	if _, err := env.service.Review(t.Context(), "weather"); !hasCode(err, ErrTampered) {
		t.Fatalf("Review error = %v, want %s", err, ErrTampered)
	}
}

func TestApproveUnknownPluginIsNotFound(t *testing.T) {
	env := newTestEnv(t)
	if _, err := env.service.Approve(t.Context(), "weather", strings.Repeat("a", 12)); !hasCode(err, ErrNotFound) {
		t.Fatalf("Approve error = %v", err)
	}
	if _, err := env.service.Approve(t.Context(), "../x", strings.Repeat("a", 12)); !hasCode(err, ErrInvalidArgument) {
		t.Fatalf("Approve traversal error = %v", err)
	}
}

func TestUpdatesFollowTheWideningRules(t *testing.T) {
	baseFiles := serverFiles()
	changedFiles := map[string]string{"server": "#!/bin/sh\nexit 1\n"}
	cases := []struct {
		name          string
		manifest      func(t *testing.T) string
		files         map[string]string
		state         string
		approved      string
		quarantines   bool
		widened       []Change
		accepted      bool
		grantsRemain  []GrantKey
		pendingTools  []string
		activeTools   []string
		heldAfterPlan ErrorCode
	}{
		{
			name:     "new tool disables only that tool",
			manifest: func(t *testing.T) string { return withTool(t, bump(t, baseManifest, "1.1.0"), "radar", "read") },
			files:    baseFiles, state: StateActive, approved: "1.0.0",
			widened:     []Change{{Surface: SurfaceTool, Value: "radar", Kind: ChangeAdded}},
			activeTools: []string{"alerts", "forecast"},
		},
		{
			name: "new egress host leaves the tools running",
			manifest: func(t *testing.T) string {
				return replaceOnce(t, bump(t, baseManifest, "1.1.0"), "[api.weather.example]", "[api.weather.example, cdn.example]")
			},
			files: baseFiles, state: StateActive, approved: "1.0.0",
			widened:     []Change{{Surface: SurfaceEgress, Value: "cdn.example", Kind: ChangeAdded}},
			activeTools: []string{"alerts", "forecast"},
		},
		{
			name: "tier raise disables that tool",
			manifest: func(t *testing.T) string {
				return replaceOnce(t, bump(t, baseManifest, "1.1.0"), "tier: read\negress", "tier: effect\negress")
			},
			files: baseFiles, state: StateActive, approved: "1.0.0",
			widened:     []Change{{Surface: SurfaceTool, Value: "alerts", Kind: ChangeChanged, Detail: DetailTierRaised}},
			activeTools: []string{"forecast"},
		},
		{
			name: "description change disables that tool",
			manifest: func(t *testing.T) string {
				return replaceOnce(t, bump(t, baseManifest, "1.1.0"), "Get a forecast for a city.", "Get a forecast.")
			},
			files: baseFiles, state: StateActive, approved: "1.0.0",
			widened:     []Change{{Surface: SurfaceTool, Value: "forecast", Kind: ChangeChanged, Detail: DetailDescriptionChanged}},
			activeTools: []string{"alerts"},
		},
		{
			name: "new capability holds the plugin until review",
			manifest: func(t *testing.T) string {
				return replaceOnce(t, bump(t, baseManifest, "1.1.0"), "[network]", "[network, workspace-read]")
			},
			files: baseFiles, state: StateActive, approved: "1.0.0",
			widened:       []Change{{Surface: SurfaceCapability, Value: "workspace-read", Kind: ChangeAdded}},
			heldAfterPlan: ErrSurfacePending,
		},
		{
			name: "backend change quarantines",
			manifest: func(t *testing.T) string {
				return replaceOnce(t, bump(t, baseManifest, "1.1.0"), "[--quiet]", "[--loud]")
			},
			files: baseFiles, state: StateQuarantined, approved: "1.0.0", quarantines: true,
		},
		{
			name:     "bundle change quarantines",
			manifest: func(t *testing.T) string { return bump(t, baseManifest, "1.1.0") },
			files:    changedFiles, state: StateQuarantined, approved: "1.0.0", quarantines: true,
		},
		{
			name: "removal needs no review",
			manifest: func(t *testing.T) string {
				return replaceOnce(t, bump(t, baseManifest, "1.1.0"), "  - name: alerts\n    description: List active alerts.\n    tier: read\n", "")
			},
			files: baseFiles, state: StateActive, approved: "1.1.0", accepted: true,
			grantsRemain: []GrantKey{{SurfaceCapability, "network"}, {SurfaceEgress, "api.weather.example"}, {SurfaceTool, "forecast"}},
			activeTools:  []string{"forecast"},
		},
		{
			name: "metadata only update needs no review",
			manifest: func(t *testing.T) string {
				return replaceOnce(t, bump(t, baseManifest, "1.0.1"), "name: Weather", "name: Weather Plus")
			},
			files: baseFiles, state: StateActive, approved: "1.0.1", accepted: true,
			activeTools: []string{"alerts", "forecast"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.approve("weather", env.install(baseManifest, baseFiles))
			result := env.install(tc.manifest(t), tc.files)
			if result.State != tc.state || result.Diff.Quarantines() != tc.quarantines || result.Accepted != tc.accepted {
				t.Fatalf("result = %+v", result)
			}
			if !slices.Equal(result.Diff.Widened, tc.widened) {
				t.Fatalf("widened = %v, want %v", result.Diff.Widened, tc.widened)
			}
			record, _ := env.registry.Get(t.Context(), "weather")
			if record.State != tc.state || record.ApprovedVersion != tc.approved {
				t.Fatalf("record = %+v", record)
			}
			if tc.grantsRemain != nil {
				grants, _ := env.registry.Grants(t.Context(), "weather")
				var keys []GrantKey
				for _, grant := range grants {
					keys = append(keys, grant.GrantKey)
				}
				if !slices.Equal(keys, tc.grantsRemain) {
					t.Fatalf("grants = %v, want %v", keys, tc.grantsRemain)
				}
			}
			plan, err := env.service.Plan(t.Context())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			switch {
			case tc.state != StateActive:
				if len(plan.Active) != 0 || len(plan.Held) != 0 {
					t.Fatalf("plan = %+v, want nothing running", plan)
				}
			case tc.heldAfterPlan != "":
				if len(plan.Active) != 0 || len(plan.Held) != 1 || plan.Held[0].Code != tc.heldAfterPlan {
					t.Fatalf("plan = %+v, want held %s", plan, tc.heldAfterPlan)
				}
			default:
				if len(plan.Active) != 1 {
					t.Fatalf("plan = %+v", plan)
				}
				var names []string
				for _, tool := range plan.Active[0].Tools {
					names = append(names, tool.Name)
				}
				if !slices.Equal(names, tc.activeTools) {
					t.Fatalf("running tools = %v, want %v", names, tc.activeTools)
				}
			}
		})
	}
}

func TestReviewShowsTheSurfaceAndTheDiffAgainstTheApproval(t *testing.T) {
	t.Setenv("WEATHER_TOKEN", "hunter2-the-secret-value")
	env := newTestEnv(t)
	manifest := `api_version: aura.plugin/v1
id: weather
version: 1.0.0
name: Weather
description: Weather forecasts.
capabilities: [network]
tools:
  - name: forecast
    description: Get a forecast for a city.
    tier: read
egress: [api.weather.example]
secrets:
  - name: token
    ref: env://WEATHER_TOKEN
auth:
  kind: static_secret
  secret: token
backend:
  kind: mcp
  transport: streamable_http
  url: https://api.weather.example/mcp
`
	first := env.install(manifest, nil)
	review, err := env.service.Review(t.Context(), "weather")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if review.Digest != first.Digest || review.State != StateQuarantined || review.Diff != nil {
		t.Fatalf("review = %+v", review)
	}
	if review.Backend.Kind != "mcp" || review.Backend.URL != "https://api.weather.example/mcp" {
		t.Fatalf("backend = %+v", review.Backend)
	}
	if len(review.Tools) != 1 || review.Tools[0].ID != "weather.forecast" || review.Tools[0].Tier != TierRead {
		t.Fatalf("tools = %+v", review.Tools)
	}
	if len(review.Secrets) != 1 || review.Secrets[0] != (ReviewSecret{Name: "token", Ref: "env://WEATHER_TOKEN"}) {
		t.Fatalf("secrets = %+v", review.Secrets)
	}
	encoded, err := json.Marshal(review)
	if err != nil || strings.Contains(string(encoded), "hunter2") {
		t.Fatalf("review leaks a secret value: %s %v", encoded, err)
	}
	env.approve("weather", first)
	update := withTool(t, bump(t, manifest, "1.1.0"), "history", "read")
	update = replaceOnce(t, update, "Get a forecast for a city.", "Get a forecast.")
	env.install(update, nil)
	review, err = env.service.Review(t.Context(), "weather")
	if err != nil {
		t.Fatalf("Review update: %v", err)
	}
	if review.ApprovedVersion != "1.0.0" || review.Version != "1.1.0" || review.Diff == nil {
		t.Fatalf("update review = %+v", review)
	}
	if len(review.Diff.Widened) != 2 || len(review.Diff.Restart) != 0 {
		t.Fatalf("diff = %+v", review.Diff)
	}
}

func TestDisableAndList(t *testing.T) {
	env := newTestEnv(t)
	result := env.install(baseManifest, serverFiles())
	env.approve("weather", result)
	if err := env.service.Disable(t.Context(), "weather"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := env.service.Disable(t.Context(), "missing"); !hasCode(err, ErrNotFound) {
		t.Fatalf("Disable unknown error = %v", err)
	}
	summaries, err := env.service.List(t.Context())
	if err != nil || len(summaries) != 1 {
		t.Fatalf("List = %v, %v", summaries, err)
	}
	want := Summary{ID: "weather", Name: "Weather", Version: "1.0.0", ApprovedVersion: "1.0.0", State: StateDisabled, DigestPrefix: result.Digest[:12]}
	if summaries[0] != want {
		t.Fatalf("summary = %+v, want %+v", summaries[0], want)
	}
	plan, err := env.service.Plan(t.Context())
	if err != nil || len(plan.Active) != 0 || len(plan.Held) != 0 {
		t.Fatalf("plan after disable = %+v, %v", plan, err)
	}
}

func TestRemoveNeedsApplyAndLeavesNothing(t *testing.T) {
	env := newTestEnv(t)
	env.approve("weather", env.install(baseManifest, serverFiles()))
	env.install(withTool(t, bump(t, baseManifest, "1.1.0"), "radar", "read"), serverFiles())
	dry, err := env.service.Remove(t.Context(), "weather", false)
	if err != nil || dry.Applied || dry.Version != "1.1.0" || dry.ApprovedVersion != "1.0.0" {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
	if _, err := env.registry.Get(t.Context(), "weather"); err != nil {
		t.Fatalf("dry run removed the record: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.dir, "weather", "1.0.0")); err != nil {
		t.Fatalf("dry run removed the bundle: %v", err)
	}
	applied, err := env.service.Remove(t.Context(), "weather", true)
	if err != nil || !applied.Applied {
		t.Fatalf("apply = %+v, %v", applied, err)
	}
	if records, _ := env.registry.List(t.Context()); len(records) != 0 {
		t.Fatalf("records after remove = %v", records)
	}
	if len(env.registry.versions) != 0 || len(env.registry.grants) != 0 {
		t.Fatalf("rows left: versions %v grants %v", env.registry.versions, env.registry.grants)
	}
	if entries := listDir(t, env.dir); len(entries) != 0 {
		t.Fatalf("plugins dir holds %v", entries)
	}
	if _, err := env.service.Remove(t.Context(), "weather", true); !hasCode(err, ErrNotFound) {
		t.Fatalf("second remove error = %v", err)
	}
}

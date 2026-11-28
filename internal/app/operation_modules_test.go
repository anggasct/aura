package app

import (
	"strings"
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/memory"
	"github.com/anggasct/aura/internal/modules/memory/memorytest"
	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/profile/profiletest"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/turn"
)

func groupNames(catalogs []operation.Catalog) []string {
	names := make([]string, 0, len(catalogs))
	for i := range catalogs {
		names = append(names, catalogs[i].Group.Name)
	}
	return names
}

func TestAServerOffersAModuleGroupOnlyWhenTheModuleIsEnabled(t *testing.T) {
	enabledProfile := profile.Default()
	cases := []struct {
		name    string
		memory  bool
		profile *profile.Config
		want    []string
	}{
		{name: "neither module", want: []string{"cron", "kb"}},
		{name: "memory only", memory: true, want: []string{"cron", "kb", "memory"}},
		{name: "profile only", profile: &enabledProfile, want: []string{"cron", "kb", "profile"}},
		{name: "a profile section that is switched off", profile: &profile.Config{}, want: []string{"cron", "kb"}},
		{name: "both modules", memory: true, profile: &enabledProfile, want: []string{"cron", "kb", "memory", "profile"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := appconfig.Default()
			cfg.Memory.Enabled = tc.memory
			cfg.Profile = tc.profile

			got := groupNames(enabledOperationCatalogs(&cfg))

			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("groups = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestADisabledProfileLeavesNoToolAndNoDescriptionLine(t *testing.T) {
	cases := []struct {
		name    string
		profile bool
	}{
		{name: "profile disabled"},
		{name: "profile enabled", profile: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := appconfig.Default()
			cfg.Memory.Enabled = true
			cfg.Profile = nil
			if tc.profile {
				enabled := profile.Default()
				cfg.Profile = &enabled
			}
			db := operationtest.OpenDatabase(t, memory.Schema(), profile.Schema())
			memoryOperations, err := memory.NewOperations(&memory.OperationDeps{Service: memorytest.NewRig(t, db).Service})
			if err != nil {
				t.Fatalf("memory.NewOperations: %v", err)
			}
			bindings := memoryOperations.Bindings()
			if tc.profile {
				profileOperations, err := profile.NewOperations(&profile.OperationDeps{Service: profiletest.NewRig(t, db).Service})
				if err != nil {
					t.Fatalf("profile.NewOperations: %v", err)
				}
				bindings = append(bindings, profileOperations.Bindings()...)
			}
			catalogs := enabledOperationCatalogs(&cfg)
			catalogs = withoutGroups(catalogs, "cron", "kb")
			rig := operationtest.NewRig(t, &operationtest.Options{
				DB: db, Catalogs: catalogs, Bindings: func(*operationtest.Stubs) []operation.Binding { return bindings },
				WithDiscovery: true,
			})

			var profileTools int
			for _, registration := range rig.Registry.Tools() {
				if strings.HasPrefix(registration.Definition.Name, "aura_profile_") {
					profileTools++
				}
			}
			description, err := rig.Discovery.Describe(&operation.DescriptionInput{
				Turn: operation.Turn{Origin: turn.OriginTerminal, Kind: operation.TurnAttended}, Search: true,
			})
			if err != nil {
				t.Fatalf("Describe: %v", err)
			}

			if !strings.Contains(description, "- memory: ") {
				t.Errorf("the description lacks the memory group:\n%s", description)
			}
			if got := strings.Contains(description, "- profile: "); got != tc.profile {
				t.Errorf("the description lists the profile group = %t, want %t:\n%s", got, tc.profile, description)
			}
			if want := map[bool]int{false: 0, true: 6}[tc.profile]; profileTools != want {
				t.Errorf("profile tools = %d, want %d", profileTools, want)
			}
		})
	}
}

func withoutGroups(catalogs []operation.Catalog, names ...string) []operation.Catalog {
	kept := make([]operation.Catalog, 0, len(catalogs))
	for i := range catalogs {
		drop := false
		for _, name := range names {
			drop = drop || catalogs[i].Group.Name == name
		}
		if !drop {
			kept = append(kept, catalogs[i])
		}
	}
	return kept
}

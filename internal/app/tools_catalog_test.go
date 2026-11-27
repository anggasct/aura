package app

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appconfig "github.com/anggasct/aura/internal/app/config"
	"github.com/anggasct/aura/internal/modules/github"
	"github.com/anggasct/aura/internal/modules/handoff"
	"github.com/anggasct/aura/internal/modules/workflow"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	"github.com/anggasct/aura/internal/platform/turn"
)

func TestStepToolNamesDropWorkflowAndHandoffTools(t *testing.T) {
	got := stepToolNames([]string{"read_file", "handoff_to_owner", "workflow_list", "workflow_start", "write_file"})
	if want := []string{"read_file", "write_file"}; !slices.Equal(got, want) {
		t.Fatalf("stepToolNames = %v, want %v", got, want)
	}
	if got := stepToolNames([]string{"github_comment"}); !slices.Equal(got, []string{"github_comment"}) {
		t.Errorf("stepToolNames kept %v, want the tool with no builtin definition kept", got)
	}
	if got := stepToolNames(nil); got != nil {
		t.Errorf("stepToolNames(nil) = %v, want nil", got)
	}
}

func TestEveryOrchestrationToolIsFlagged(t *testing.T) {
	builtins := appconfig.BuiltinToolDefinitions()
	for i := range builtins {
		name := builtins[i].Name
		want := strings.HasPrefix(name, "workflow_") || name == handoff.ToolName
		if builtins[i].Orchestration != want {
			t.Errorf("%s Orchestration = %t, want %t", name, builtins[i].Orchestration, want)
		}
	}
}

func TestStaticCatalogIgnoresTheCapabilitySet(t *testing.T) {
	if got, want := len(definitionNames()), 16; got != want {
		t.Fatalf("definitionNames() = %d names, want %d", got, want)
	}
	if got, want := len(catalogDefinitions()), 16; got != want {
		t.Fatalf("catalogDefinitions() = %d entries, want %d", got, want)
	}
}

func TestOnlyTheSixWorkflowDefinitionsAreOwnerOptIn(t *testing.T) {
	workflowNames := []string{}
	for _, definition := range workflow.ToolDefinitions() {
		workflowNames = append(workflowNames, definition.Name)
		if !definition.OwnerOptIn {
			t.Errorf("%s OwnerOptIn = false, want true", definition.Name)
		}
	}
	if len(workflowNames) != 6 {
		t.Fatalf("workflow definitions = %v, want 6", workflowNames)
	}
	for _, definition := range github.Definitions() {
		if definition.OwnerOptIn {
			t.Errorf("%s OwnerOptIn = true, want false", definition.Name)
		}
	}
	if definition := handoff.ToolDefinition(); definition.OwnerOptIn {
		t.Errorf("%s OwnerOptIn = true, want false", definition.Name)
	}
	builtins := appconfig.BuiltinToolDefinitions()
	for i := range builtins {
		if want := slices.Contains(workflowNames, builtins[i].Name); builtins[i].OwnerOptIn != want {
			t.Errorf("%s OwnerOptIn = %t, want %t", builtins[i].Name, builtins[i].OwnerOptIn, want)
		}
	}
}

func TestCatalogDefinitionsCarryTheOwnerOptInFlag(t *testing.T) {
	builtins := appconfig.BuiltinToolDefinitions()
	catalog := catalogDefinitions()
	if len(catalog) != len(builtins) {
		t.Fatalf("catalogDefinitions() = %d entries, want %d", len(catalog), len(builtins))
	}
	ownerOptIn := 0
	for i := range catalog {
		if catalog[i].OwnerOptIn != builtins[i].OwnerOptIn {
			t.Errorf("%s OwnerOptIn = %t, want %t", catalog[i].Name, catalog[i].OwnerOptIn, builtins[i].OwnerOptIn)
		}
		if catalog[i].OwnerOptIn {
			ownerOptIn++
		}
	}
	if ownerOptIn != 6 {
		t.Fatalf("owner opt-in catalog definitions = %d, want 6", ownerOptIn)
	}
}

func TestNewChannelEffectsBuildsExecutor(t *testing.T) {
	if _, err := newChannelEffects(nil, nil); err == nil {
		t.Fatal("nil database accepted")
	}
	dataRoot := t.TempDir()
	sqlitetest.Seed(t, filepath.Join(dataRoot, "aura.db"), Schemas()...)
	db, err := sqlite.Open(t.Context(), filepath.Join(dataRoot, "aura.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := newChannelEffects(db, nil); err != nil {
		t.Fatalf("newChannelEffects: %v", err)
	}
}

func TestNewRefusesANilLiveEventsPublisher(t *testing.T) {
	dataRoot := t.TempDir()
	sqlitetest.Seed(t, filepath.Join(dataRoot, "aura.db"), Schemas()...)
	db, err := sqlite.Open(t.Context(), filepath.Join(dataRoot, "aura.db"), sqlite.Options{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := appconfig.Default()
	cfg.Tools.Workspace = t.TempDir()
	_, executor, _, err := newToolBroker(&toolExecutorInput{
		result:       &appconfig.LoadResult{Config: &cfg, CapabilityReport: capabilitytest.All(t)},
		db:           db,
		artifactRoot: dataRoot,
		wiring:       &toolWiring{},
	})
	if err == nil {
		t.Fatal("newToolBroker accepted a nil live events publisher")
	}
	if executor != nil {
		t.Fatalf("newToolBroker returned an executor with a nil live events publisher: %v", executor)
	}
	if code, ok := fault.CodeOf[turn.ErrorCode](err); !ok || code != turn.ErrorCodeInvalidArgument {
		t.Fatalf("code = %q (%v), want %q", code, ok, turn.ErrorCodeInvalidArgument)
	}
}

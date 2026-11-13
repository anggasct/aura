package toolsbuiltin

import (
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/tools"
)

func TestStepToolNamesDropWorkflowAndHandoffTools(t *testing.T) {
	got := StepToolNames([]string{"read_file", "handoff_to_owner", "workflow_list", "workflow_start", "write_file"})
	if want := []string{"read_file", "write_file"}; !slices.Equal(got, want) {
		t.Fatalf("StepToolNames = %v, want %v", got, want)
	}
	if got := StepToolNames([]string{"github_comment"}); !slices.Equal(got, []string{"github_comment"}) {
		t.Errorf("StepToolNames kept %v, want the tool with no builtin definition kept", got)
	}
	if got := StepToolNames(nil); got != nil {
		t.Errorf("StepToolNames(nil) = %v, want nil", got)
	}
}

func TestEveryOrchestrationToolIsFlagged(t *testing.T) {
	builtins := tools.Builtins()
	for i := range builtins {
		name := builtins[i].Name
		want := strings.HasPrefix(name, "workflow_") || name == tools.HandoffToolName
		if builtins[i].Orchestration != want {
			t.Errorf("%s Orchestration = %t, want %t", name, builtins[i].Orchestration, want)
		}
	}
}

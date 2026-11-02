package toolsbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
)

type scriptedWorkflows struct {
	callers []WorkflowCaller
	calls   []string
	err     error
}

func (s *scriptedWorkflows) record(caller *WorkflowCaller, call string) {
	s.callers = append(s.callers, *caller)
	s.calls = append(s.calls, call)
}

func (s *scriptedWorkflows) List(_ context.Context, caller *WorkflowCaller) ([]WorkflowInfo, error) {
	s.record(caller, "list")
	return nil, s.err
}

func (s *scriptedWorkflows) Start(_ context.Context, caller *WorkflowCaller, id, objective string) (WorkflowRunState, error) {
	s.record(caller, "start "+id+" "+objective)
	return WorkflowRunState{RunID: "run_1", WorkflowID: id, Status: "queued"}, s.err
}

func (s *scriptedWorkflows) Status(_ context.Context, caller *WorkflowCaller, runID string) (WorkflowRunState, error) {
	s.record(caller, "status "+runID)
	return WorkflowRunState{RunID: runID, Status: "running", Steps: []WorkflowStepState{{ID: "hold", Status: "running"}}}, s.err
}

func (s *scriptedWorkflows) Cancel(_ context.Context, caller *WorkflowCaller, runID string) (WorkflowRunState, error) {
	s.record(caller, "cancel "+runID)
	return WorkflowRunState{RunID: runID, Status: "cancelled"}, s.err
}

func workflowRequest(class principal.Class, tool, args string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{
		SessionID: "lobby/discord:owner/dm:601", PrincipalID: "discord:601", PrincipalClass: class, AgentID: "lobby",
		ToolName: tool, ToolVersion: tools.Version, Arguments: json.RawMessage(args),
	}
}

func runWorkflowTool(t *testing.T, holder *workflowHolder, request *toolbroker.ToolRequest) (toolbroker.ToolResult, error) {
	t.Helper()
	return holder.adapters()[request.ToolName+"@"+tools.Version](t.Context(), request, approval.Constraints{})
}

func TestWorkflowAdaptersNeedTheDurableRuntime(t *testing.T) {
	holder := &workflowHolder{}
	for _, tool := range []string{tools.WorkflowListToolName, tools.WorkflowStartToolName, tools.WorkflowStatusToolName, tools.WorkflowCancelToolName} {
		_, err := runWorkflowTool(t, holder, workflowRequest(principal.ClassOwner, tool, `{"id":"x","run_id":"r"}`))
		if code, ok := toolbroker.CodeOf(err); !ok || code != toolbroker.ResultCapabilityUnavailable || !strings.Contains(err.Error(), "durable runtime") {
			t.Fatalf("%s without a controller = %v", tool, err)
		}
	}
}

func TestWorkflowAdaptersPassTheCallerAndShapeResults(t *testing.T) {
	holder := &workflowHolder{}
	controller := &scriptedWorkflows{}
	(&Executor{workflows: holder}).SetWorkflowController(controller)
	result, err := runWorkflowTool(t, holder, workflowRequest(principal.ClassPublic, tools.WorkflowListToolName, `{}`))
	if err != nil || string(result.Output) != `{"workflows":[]}` {
		t.Fatalf("list = %s, %v", result.Output, err)
	}
	result, err = runWorkflowTool(t, holder, workflowRequest(principal.ClassPublic, tools.WorkflowStartToolName, `{"id":"book_visit","objective":"Saturday"}`))
	if err != nil || string(result.Output) != `{"run_id":"run_1","workflow_id":"book_visit","status":"queued"}` {
		t.Fatalf("start = %s, %v", result.Output, err)
	}
	result, err = runWorkflowTool(t, holder, workflowRequest(principal.ClassPublic, tools.WorkflowStatusToolName, `{"run_id":"run_1"}`))
	if err != nil || string(result.Output) != `{"run_id":"run_1","status":"running","steps":[{"id":"hold","status":"running"}]}` {
		t.Fatalf("status = %s, %v", result.Output, err)
	}
	if _, err := runWorkflowTool(t, holder, workflowRequest("", tools.WorkflowCancelToolName, `{"run_id":"run_1"}`)); err != nil {
		t.Fatal(err)
	}
	want := WorkflowCaller{SessionID: "lobby/discord:owner/dm:601", PrincipalID: "discord:601", PrincipalClass: principal.ClassPublic, AgentID: "lobby"}
	if controller.callers[0] != want || controller.callers[3].PrincipalClass != principal.ClassOwner {
		t.Fatalf("callers = %+v", controller.callers)
	}
	if strings.Join(controller.calls, "|") != "list|start book_visit Saturday|status run_1|cancel run_1" {
		t.Fatalf("calls = %v", controller.calls)
	}
}

func TestWorkflowAdaptersMapControllerErrors(t *testing.T) {
	holder := &workflowHolder{}
	controller := &scriptedWorkflows{}
	(&Executor{workflows: holder}).SetWorkflowController(controller)
	for _, tc := range []struct {
		err   error
		class toolbroker.ResultClass
		code  string
	}{
		{ErrWorkflowNotPermitted, toolbroker.ResultPolicyDenied, WorkflowCodeNotPermitted},
		{ErrWorkflowRunNotFound, toolbroker.ResultInvalidArgument, WorkflowCodeRunNotFound},
		{errors.New("ingress unreachable"), toolbroker.ResultExecutionFailed, "workflow request failed"},
	} {
		controller.err = tc.err
		_, err := runWorkflowTool(t, holder, workflowRequest(principal.ClassTrusted, tools.WorkflowStartToolName, `{"id":"refund"}`))
		if code, ok := toolbroker.CodeOf(err); !ok || code != tc.class || !strings.Contains(err.Error(), tc.code) {
			t.Fatalf("controller error %v surfaced as %v", tc.err, err)
		}
	}
}

func TestWorkflowDefinitionsAreReadTierOptInAndStrict(t *testing.T) {
	for _, name := range []string{tools.WorkflowListToolName, tools.WorkflowStartToolName, tools.WorkflowStatusToolName, tools.WorkflowCancelToolName} {
		definition := tools.DefinitionsByKey()[name+"@"+tools.Version]
		if definition.Effectful || definition.RequiresApproval || !definition.OptIn || tools.EffectTier(name) {
			t.Fatalf("%s = %+v, want read tier and opt-in", name, definition)
		}
	}
	for tool, cases := range map[string]map[string]bool{
		tools.WorkflowListToolName: {`{}`: true, `{"id":"x"}`: false},
		tools.WorkflowStartToolName: {
			`{"id":"book_visit"}`: true, `{"id":"book_visit","objective":"Saturday"}`: true,
			`{}`: false, `{"id":""}`: false, `{"id":"x","version":2}`: false, `{"id":"x","permissions":["exec"]}`: false,
			`{"id":"x","objective":7}`: false, `{"id":"x","objective":"` + strings.Repeat("é", tools.MaxWorkflowObjectiveRunes+1) + `"}`: false,
			`{"id":"x","objective":"` + strings.Repeat("é", tools.MaxWorkflowObjectiveRunes) + `"}`: true,
		},
		tools.WorkflowStatusToolName: {`{"run_id":"run_1"}`: true, `{}`: false, `{"run_id":"r","id":"x"}`: false},
		tools.WorkflowCancelToolName: {`{"run_id":"run_1"}`: true, `{"run_id":""}`: false},
	} {
		definition := tools.DefinitionsByKey()[tool+"@"+tools.Version]
		for raw, valid := range cases {
			if _, err := definition.Validate(json.RawMessage(raw)); (err == nil) != valid {
				t.Errorf("%s Validate(%.60s) err = %v, want valid=%t", tool, raw, err, valid)
			}
		}
	}
}

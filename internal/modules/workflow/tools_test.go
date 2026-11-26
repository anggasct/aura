package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

type scriptedWorkflows struct {
	callers []Caller
	calls   []string
	err     error
}

func (s *scriptedWorkflows) record(caller *Caller, call string) {
	s.callers = append(s.callers, *caller)
	s.calls = append(s.calls, call)
}

func (s *scriptedWorkflows) List(_ context.Context, caller *Caller) ([]Info, error) {
	s.record(caller, "list")
	return nil, s.err
}

func (s *scriptedWorkflows) Start(_ context.Context, caller *Caller, id, objective string) (RunState, error) {
	s.record(caller, "start "+id+" "+objective)
	return RunState{RunID: "run_1", WorkflowID: id, Status: "queued"}, s.err
}

func (s *scriptedWorkflows) Propose(_ context.Context, caller *Caller, spec json.RawMessage) (ProposalSummary, error) {
	s.record(caller, "propose "+string(spec))
	return ProposalSummary{ProposalID: "prop_1", Digest: "abc", Plan: "Plan: x", UntrustedInput: true, ApproveWith: "aura workflow proposal approve prop_1"}, s.err
}

func (s *scriptedWorkflows) SaveRoutine(_ context.Context, caller *Caller, runID, name string) (RoutineSummary, error) {
	s.record(caller, "save "+runID+" "+name)
	return RoutineSummary{ID: "routine_weekly", Version: 2}, s.err
}

func (s *scriptedWorkflows) Status(_ context.Context, caller *Caller, runID string) (RunState, error) {
	s.record(caller, "status "+runID)
	return RunState{RunID: runID, Status: "running", Steps: []StepState{{ID: "hold", Status: "running"}}}, s.err
}

func (s *scriptedWorkflows) Cancel(_ context.Context, caller *Caller, runID string) (RunState, error) {
	s.record(caller, "cancel "+runID)
	return RunState{RunID: runID, Status: "cancelled"}, s.err
}

func workflowToolRequest(class principal.Class, tool, args string) *toolbroker.ToolRequest {
	return &toolbroker.ToolRequest{
		SessionID:      "lobby/discord:owner/dm:601",
		PrincipalID:    "discord:601",
		PrincipalClass: class,
		AgentID:        "lobby",
		ToolName:       tool,
		ToolVersion:    tools.Version,
		Arguments:      json.RawMessage(args),
	}
}

func runWorkflowTool(t *testing.T, provider toolbroker.ToolProvider, request *toolbroker.ToolRequest) (toolbroker.ToolResult, error) {
	t.Helper()
	regs := provider.Tools()
	for i := range regs {
		if regs[i].Definition.Name == request.ToolName && regs[i].Definition.Version == request.ToolVersion {
			return regs[i].Adapter(t.Context(), request, approval.Constraints{})
		}
	}
	t.Fatalf("tool %s@%s not registered by provider", request.ToolName, request.ToolVersion)
	return toolbroker.ToolResult{}, nil
}

func TestNewToolProviderRejectsNilController(t *testing.T) {
	provider, err := NewToolProvider(nil)
	if provider != nil || !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewToolProvider(nil) = %v, %v; want %s", provider, err, ErrorCodeInvalidArgument)
	}
	broker, err := toolbroker.New(&toolbroker.Options{Adapters: map[string]toolbroker.Adapter{}})
	if err != nil {
		t.Fatalf("toolbroker.New: %v", err)
	}
	for name, args := range map[string]string{
		tools.WorkflowListToolName:   `{}`,
		tools.WorkflowStartToolName:  `{"id":"x"}`,
		tools.WorkflowStatusToolName: `{"run_id":"r"}`,
		tools.WorkflowCancelToolName: `{"run_id":"r"}`,
	} {
		req := workflowToolRequest(principal.ClassOwner, name, args)
		req.RequestID = "req-" + name
		req.TurnID = "turn-1"
		req.Trust = approval.TrustOwnerInput
		_, err := broker.Execute(t.Context(), req)
		if !fault.HasCode(err, toolbroker.ResultCapabilityUnavailable) {
			t.Fatalf("%s on broker without workflow provider = %v, want %s", name, err, toolbroker.ResultCapabilityUnavailable)
		}
	}
}

func TestWorkflowToolsMatchBuiltinCatalog(t *testing.T) {
	catalog := tools.DefinitionsByKey()
	policy := toolbroker.DefaultPolicy().Rules
	defs := ToolDefinitions()
	rules := ToolRules()
	if len(defs) != 6 || len(rules) != 6 {
		t.Fatalf("definitions=%d rules=%d, want 6 each", len(defs), len(rules))
	}

	payloadMatrix := map[string][]struct {
		name string
		raw  string
		ok   bool
	}{
		tools.WorkflowListToolName: {
			{name: "empty object", raw: `{}`, ok: true},
			{name: "unknown key", raw: `{"id":"x"}`, ok: false},
			{name: "not object", raw: `[]`, ok: false},
			{name: "trailing data", raw: `{} {}`, ok: false},
		},
		tools.WorkflowStartToolName: {
			{name: "id only", raw: `{"id":"book_visit"}`, ok: true},
			{name: "id and objective", raw: `{"id":"book_visit","objective":"Saturday"}`, ok: true},
			{name: "max objective runes", raw: `{"id":"x","objective":"` + strings.Repeat("é", tools.MaxWorkflowObjectiveRunes) + `"}`, ok: true},
			{name: "over max objective runes", raw: `{"id":"x","objective":"` + strings.Repeat("é", tools.MaxWorkflowObjectiveRunes+1) + `"}`, ok: false},
			{name: "missing id", raw: `{}`, ok: false},
			{name: "empty id", raw: `{"id":""}`, ok: false},
			{name: "non-string objective", raw: `{"id":"x","objective":7}`, ok: false},
			{name: "unknown key", raw: `{"id":"x","version":2}`, ok: false},
		},
		tools.WorkflowStatusToolName: {
			{name: "valid", raw: `{"run_id":"run_1"}`, ok: true},
			{name: "missing run_id", raw: `{}`, ok: false},
			{name: "empty run_id", raw: `{"run_id":""}`, ok: false},
			{name: "unknown key", raw: `{"run_id":"r","id":"x"}`, ok: false},
		},
		tools.WorkflowCancelToolName: {
			{name: "valid", raw: `{"run_id":"run_1"}`, ok: true},
			{name: "empty run_id", raw: `{"run_id":""}`, ok: false},
			{name: "unknown key", raw: `{"run_id":"r","force":true}`, ok: false},
		},
		tools.WorkflowProposeToolName: {
			{name: "valid", raw: `{"spec":{"goal":"g","steps":[{"id":"a"}]}}`, ok: true},
			{name: "missing spec", raw: `{}`, ok: false},
			{name: "empty spec", raw: `{"spec":{}}`, ok: false},
			{name: "string spec", raw: `{"spec":"text"}`, ok: false},
			{name: "unknown key", raw: `{"spec":{"goal":"g"},"run":true}`, ok: false},
		},
		tools.WorkflowSaveRoutineToolName: {
			{name: "valid", raw: `{"run_id":"run_1","name":"weekly report"}`, ok: true},
			{name: "missing name", raw: `{"run_id":"run_1"}`, ok: false},
			{name: "missing run_id", raw: `{"name":"x"}`, ok: false},
			{name: "unknown key", raw: `{"run_id":"r","name":"x","version":2}`, ok: false},
		},
	}

	for i := range defs {
		got := defs[i]
		wantPtr, ok := catalog[got.Key()]
		if !ok {
			t.Fatalf("%s missing from builtin catalog", got.Key())
		}
		if got.Validator == nil {
			t.Fatalf("%s Validator must not be nil", got.Key())
		}
		gotCopy := got
		gotCopy.Validator = nil
		wantCopy := *wantPtr
		wantCopy.Validator = nil
		if !reflect.DeepEqual(gotCopy, wantCopy) {
			t.Fatalf("%s definition mismatch:\n got: %+v\nwant: %+v", got.Key(), gotCopy, wantCopy)
		}
		wantRule, ok := policy[got.Name]
		if !ok {
			t.Fatalf("%s missing from DefaultPolicy", got.Name)
		}
		if !reflect.DeepEqual(rules[i], wantRule) {
			t.Fatalf("%s rule mismatch:\n got: %+v\nwant: %+v", got.Name, rules[i], wantRule)
		}
		for _, tc := range payloadMatrix[got.Name] {
			t.Run(got.Name+"/"+tc.name, func(t *testing.T) {
				gotOut, gotErr := got.Validate(json.RawMessage(tc.raw))
				wantOut, wantErr := wantPtr.Validate(json.RawMessage(tc.raw))
				if (gotErr == nil) != tc.ok {
					t.Fatalf("got.Validate(%s) err = %v, want ok=%t", tc.raw, gotErr, tc.ok)
				}
				if (gotErr == nil) != (wantErr == nil) {
					t.Fatalf("parity mismatch for %s: gotErr=%v wantErr=%v", tc.raw, gotErr, wantErr)
				}
				if tc.ok && !bytes.Equal(gotOut, wantOut) {
					t.Fatalf("validated output mismatch: got %s, want %s", gotOut, wantOut)
				}
			})
		}
	}
}

func TestWorkflowAdaptersPassTheCallerAndShapeResults(t *testing.T) {
	controller := &scriptedWorkflows{}
	provider, err := NewToolProvider(controller)
	if err != nil {
		t.Fatalf("NewToolProvider: %v", err)
	}
	result, err := runWorkflowTool(t, provider, workflowToolRequest(principal.ClassPublic, tools.WorkflowListToolName, `{}`))
	if err != nil || string(result.Output) != `{"workflows":[]}` {
		t.Fatalf("list = %s, %v", result.Output, err)
	}
	result, err = runWorkflowTool(t, provider, workflowToolRequest(principal.ClassPublic, tools.WorkflowStartToolName, `{"id":"book_visit","objective":"Saturday"}`))
	if err != nil || string(result.Output) != `{"run_id":"run_1","workflow_id":"book_visit","status":"queued"}` {
		t.Fatalf("start = %s, %v", result.Output, err)
	}
	result, err = runWorkflowTool(t, provider, workflowToolRequest(principal.ClassPublic, tools.WorkflowStatusToolName, `{"run_id":"run_1"}`))
	if err != nil || string(result.Output) != `{"run_id":"run_1","status":"running","steps":[{"id":"hold","status":"running"}]}` {
		t.Fatalf("status = %s, %v", result.Output, err)
	}
	if _, err := runWorkflowTool(t, provider, workflowToolRequest("", tools.WorkflowCancelToolName, `{"run_id":"run_1"}`)); err != nil {
		t.Fatal(err)
	}
	want := Caller{SessionID: "lobby/discord:owner/dm:601", PrincipalID: "discord:601", PrincipalClass: principal.ClassPublic, AgentID: "lobby"}
	if controller.callers[0] != want || controller.callers[3].PrincipalClass != principal.ClassOwner {
		t.Fatalf("callers = %+v", controller.callers)
	}
	if strings.Join(controller.calls, "|") != "list|start book_visit Saturday|status run_1|cancel run_1" {
		t.Fatalf("calls = %v", controller.calls)
	}
}

func TestWorkflowProposeAdapterPassesTheSpecAndMapsRejections(t *testing.T) {
	controller := &scriptedWorkflows{}
	provider, err := NewToolProvider(controller)
	if err != nil {
		t.Fatalf("NewToolProvider: %v", err)
	}
	result, err := runWorkflowTool(t, provider, workflowToolRequest(principal.ClassOwner, tools.WorkflowProposeToolName, `{"spec":{"goal":"g","steps":[]}}`))
	if err != nil || string(result.Output) != `{"proposal_id":"prop_1","digest":"abc","plan":"Plan: x","untrusted_input":true,"approve_with":"aura workflow proposal approve prop_1"}` {
		t.Fatalf("propose = %s, %v", result.Output, err)
	}
	if controller.calls[0] != `propose {"goal":"g","steps":[]}` {
		t.Fatalf("calls = %v", controller.calls)
	}
	controller.err = codedError(ErrorCodeCapabilityExceeded, "step uses exec")
	_, err = runWorkflowTool(t, provider, workflowToolRequest(principal.ClassOwner, tools.WorkflowProposeToolName, `{"spec":{"goal":"g","steps":[]}}`))
	if !fault.HasCode(err, toolbroker.ResultInvalidArgument) || !strings.Contains(err.Error(), "workflow_capability_exceeded: step uses exec") {
		t.Fatalf("rejection surfaced as %v", err)
	}
}

func TestWorkflowSaveRoutineAdapterPassesRunAndName(t *testing.T) {
	controller := &scriptedWorkflows{}
	provider, err := NewToolProvider(controller)
	if err != nil {
		t.Fatalf("NewToolProvider: %v", err)
	}
	result, err := runWorkflowTool(t, provider, workflowToolRequest(principal.ClassOwner, tools.WorkflowSaveRoutineToolName, `{"run_id":"run_9","name":"weekly report"}`))
	if err != nil || string(result.Output) != `{"id":"routine_weekly","version":2}` || controller.calls[0] != "save run_9 weekly report" {
		t.Fatalf("save routine = %s, %v, calls %v", result.Output, err, controller.calls)
	}
}

func TestWorkflowAdaptersMapControllerErrors(t *testing.T) {
	controller := &scriptedWorkflows{}
	provider, err := NewToolProvider(controller)
	if err != nil {
		t.Fatalf("NewToolProvider: %v", err)
	}
	for _, tc := range []struct {
		err   error
		class toolbroker.ResultClass
		code  string
	}{
		{codedError(ErrorCodeNotPermitted, "denied"), toolbroker.ResultPolicyDenied, string(ErrorCodeNotPermitted)},
		{codedError(ErrorCodeRunNotFound, "missing"), toolbroker.ResultInvalidArgument, string(ErrorCodeRunNotFound)},
		{errors.New("ingress unreachable"), toolbroker.ResultExecutionFailed, "workflow request failed"},
	} {
		controller.err = tc.err
		_, err := runWorkflowTool(t, provider, workflowToolRequest(principal.ClassTrusted, tools.WorkflowStartToolName, `{"id":"refund"}`))
		if !fault.HasCode(err, tc.class) || !strings.Contains(err.Error(), tc.code) {
			t.Fatalf("controller error %v surfaced as %v", tc.err, err)
		}
	}
}

func TestWorkflowDefinitionsAreReadTierOptInAndStrict(t *testing.T) {
	byName := map[string]tools.Definition{}
	for _, def := range ToolDefinitions() {
		byName[def.Name] = def
		if def.Effectful || def.RequiresApproval || !def.OptIn || tools.EffectTier(def.Name) {
			t.Fatalf("%s = %+v, want read tier and opt-in", def.Name, def)
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
		tools.WorkflowSaveRoutineToolName: {
			`{"run_id":"run_1","name":"weekly report"}`: true,
			`{"run_id":"run_1"}`:                        false, `{"name":"x"}`: false, `{"run_id":"r","name":"x","version":2}`: false,
		},
		tools.WorkflowProposeToolName: {
			`{"spec":{"goal":"g","steps":[{"id":"a"}]}}`: true,
			`{}`: false, `{"spec":{}}`: false, `{"spec":"text"}`: false, `{"spec":{"goal":"g"},"run":true}`: false,
		},
	} {
		definition := byName[tool]
		for raw, valid := range cases {
			if _, err := definition.Validate(json.RawMessage(raw)); (err == nil) != valid {
				t.Errorf("%s Validate(%.60s) err = %v, want valid=%t", tool, raw, err, valid)
			}
		}
	}
}

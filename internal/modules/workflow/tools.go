package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

const (
	ListToolName        = "workflow_list"
	StartToolName       = "workflow_start"
	StatusToolName      = "workflow_status"
	CancelToolName      = "workflow_cancel"
	ProposeToolName     = "workflow_propose"
	SaveRoutineToolName = "workflow_save_routine"

	MaxObjectiveRunes = 2000
)

type ToolController interface {
	SaveRoutine(ctx context.Context, caller *Caller, runID, name string) (RoutineSummary, error)
	Propose(ctx context.Context, caller *Caller, spec json.RawMessage) (ProposalSummary, error)
	List(ctx context.Context, caller *Caller) ([]Info, error)
	Start(ctx context.Context, caller *Caller, id, objective string) (RunState, error)
	Status(ctx context.Context, caller *Caller, runID string) (RunState, error)
	Cancel(ctx context.Context, caller *Caller, runID string) (RunState, error)
}

func ToolDefinitions() []tools.Definition {
	return []tools.Definition{
		{
			Name:          ListToolName,
			Version:       tools.Version,
			Schema:        []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_list/v1","type":"object","properties":{},"additionalProperties":false}`),
			OptIn:         true,
			Orchestration: true,
			Validator:     validateWorkflowListArgs,
		},
		{
			Name:          StartToolName,
			Version:       tools.Version,
			Schema:        []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_start/v1","type":"object","properties":{"id":{"type":"string","minLength":1},"objective":{"type":"string","maxLength":2000}},"required":["id"],"additionalProperties":false}`),
			OptIn:         true,
			Orchestration: true,
			ExternalSink:  true,
			Validator:     validateWorkflowStartArgs,
		},
		{
			Name:          StatusToolName,
			Version:       tools.Version,
			Schema:        []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_status/v1","type":"object","properties":{"run_id":{"type":"string","minLength":1}},"required":["run_id"],"additionalProperties":false}`),
			OptIn:         true,
			Orchestration: true,
			Validator:     validateWorkflowRunIDArgs,
		},
		{
			Name:          ProposeToolName,
			Version:       tools.Version,
			Schema:        []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_propose/v1","type":"object","properties":{"spec":{"type":"object","properties":{"goal":{"type":"string","minLength":1},"steps":{"type":"array","minItems":1}},"required":["goal","steps"]}},"required":["spec"],"additionalProperties":false}`),
			OptIn:         true,
			Orchestration: true,
			ExternalSink:  true,
			Validator:     validateWorkflowProposeArgs,
		},
		{
			Name:          SaveRoutineToolName,
			Version:       tools.Version,
			Schema:        []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_save_routine/v1","type":"object","properties":{"run_id":{"type":"string","minLength":1},"name":{"type":"string","minLength":1,"maxLength":64}},"required":["run_id","name"],"additionalProperties":false}`),
			OptIn:         true,
			Orchestration: true,
			Validator:     validateWorkflowSaveRoutineArgs,
		},
		{
			Name:          CancelToolName,
			Version:       tools.Version,
			Schema:        []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"aura://tools/workflow_cancel/v1","type":"object","properties":{"run_id":{"type":"string","minLength":1}},"required":["run_id"],"additionalProperties":false}`),
			OptIn:         true,
			Orchestration: true,
			Validator:     validateWorkflowRunIDArgs,
		},
	}
}

func ToolRules() []approval.Rule {
	defs := ToolDefinitions()
	rules := make([]approval.Rule, 0, len(defs))
	for i := range defs {
		rules = append(rules, approval.Rule{
			ToolName:    defs[i].Name,
			ToolVersion: defs[i].Version,
			AllowedTrust: []approval.TrustLabel{
				approval.TrustOwnerInput,
				approval.TrustTrustedConfiguration,
				approval.TrustDerivedUntrusted,
			},
			Constraints: approval.Constraints{
				MaxOutputBytes: 64 * 1024,
				Timeout:        30 * time.Second,
			},
		})
	}
	return rules
}

type toolProvider struct {
	controller ToolController
}

func NewToolProvider(controller ToolController) (toolbroker.ToolProvider, error) {
	if controller == nil {
		return nil, codedError(ErrorCodeInvalidArgument, "workflow tool provider requires a controller")
	}
	return &toolProvider{controller: controller}, nil
}

func (p *toolProvider) Tools() []toolbroker.ToolRegistration {
	defs := ToolDefinitions()
	rules := ToolRules()
	adapters := workflowToolAdapters(p.controller)
	registrations := make([]toolbroker.ToolRegistration, 0, len(defs))
	for i := range defs {
		registrations = append(registrations, toolbroker.ToolRegistration{
			Definition: defs[i],
			Adapter:    adapters[defs[i].Name],
			Rule:       rules[i],
		})
	}
	return registrations
}

type workflowToolArgs struct {
	ID        string          `json:"id"`
	Objective string          `json:"objective"`
	RunID     string          `json:"run_id"`
	Spec      json.RawMessage `json:"spec"`
	Name      string          `json:"name"`
}

type workflowToolCall func(ctx context.Context, controller ToolController, caller *Caller, args workflowToolArgs) (any, error)

func workflowToolAdapters(controller ToolController) map[string]toolbroker.Adapter {
	return map[string]toolbroker.Adapter{
		ListToolName: workflowToolAdapter(controller, func(ctx context.Context, ctrl ToolController, caller *Caller, _ workflowToolArgs) (any, error) {
			workflows, err := ctrl.List(ctx, caller)
			if workflows == nil {
				workflows = []Info{}
			}
			return map[string][]Info{"workflows": workflows}, err
		}),
		StartToolName: workflowToolAdapter(controller, func(ctx context.Context, ctrl ToolController, caller *Caller, args workflowToolArgs) (any, error) {
			return ctrl.Start(ctx, caller, args.ID, args.Objective)
		}),
		StatusToolName: workflowToolAdapter(controller, func(ctx context.Context, ctrl ToolController, caller *Caller, args workflowToolArgs) (any, error) {
			return ctrl.Status(ctx, caller, args.RunID)
		}),
		ProposeToolName: workflowToolAdapter(controller, func(ctx context.Context, ctrl ToolController, caller *Caller, args workflowToolArgs) (any, error) {
			return ctrl.Propose(ctx, caller, args.Spec)
		}),
		SaveRoutineToolName: workflowToolAdapter(controller, func(ctx context.Context, ctrl ToolController, caller *Caller, args workflowToolArgs) (any, error) {
			return ctrl.SaveRoutine(ctx, caller, args.RunID, args.Name)
		}),
		CancelToolName: workflowToolAdapter(controller, func(ctx context.Context, ctrl ToolController, caller *Caller, args workflowToolArgs) (any, error) {
			return ctrl.Cancel(ctx, caller, args.RunID)
		}),
	}
}

func workflowToolAdapter(controller ToolController, call workflowToolCall) toolbroker.Adapter {
	return func(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		if request == nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
		}
		var args workflowToolArgs
		if err := json.Unmarshal(request.Arguments, &args); err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "arguments must be a JSON object")
		}
		class := request.PrincipalClass
		if class == "" {
			class = principal.ClassOwner
		}
		result, err := call(ctx, controller, &Caller{
			SessionID:      request.SessionID,
			PrincipalID:    request.PrincipalID,
			PrincipalClass: class,
			AgentID:        request.AgentID,
			Tainted:        request.Tainted,
		}, args)
		switch {
		case fault.HasCode(err, ErrorCodeNotPermitted):
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultPolicyDenied, "%s: this workflow is not available here", ErrorCodeNotPermitted)
		case fault.HasCode(err, ErrorCodeRunNotFound):
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "%s: no such workflow run", ErrorCodeRunNotFound)
		case hasWorkflowErrorCode(err):
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "%s", err.Error())
		case err != nil:
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultExecutionFailed, "workflow request failed: %v", err)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return toolbroker.ToolResult{}, err
		}
		return toolbroker.ToolResult{Class: toolbroker.ResultOK, Output: encoded}, nil
	}
}

func hasWorkflowErrorCode(err error) bool {
	_, ok := fault.CodeOf[ErrorCode](err)
	return ok
}

func decodeToolObject(raw json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("arguments must be valid JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("arguments must contain one JSON value")
		}
		return nil, fmt.Errorf("arguments have trailing data: %w", err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("arguments must be a JSON object")
	}
	return object, nil
}

func rejectUnknownToolKeys(object map[string]any, allowed ...string) error {
	for key := range object {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("unknown argument %q", key)
		}
	}
	return nil
}

func requireToolString(object map[string]any, key string) error {
	raw, ok := object[key]
	if !ok {
		return fmt.Errorf("argument %q is required", key)
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return fmt.Errorf("argument %q must be a non-empty string", key)
	}
	return nil
}

func validateWorkflowListArgs(raw json.RawMessage) (json.RawMessage, error) {
	object, err := decodeToolObject(raw)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownToolKeys(object); err != nil {
		return nil, err
	}
	return json.Marshal(object)
}

func validateWorkflowStartArgs(raw json.RawMessage) (json.RawMessage, error) {
	object, err := decodeToolObject(raw)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownToolKeys(object, "id", "objective"); err != nil {
		return nil, err
	}
	if err := requireToolString(object, "id"); err != nil {
		return nil, err
	}
	if objective, ok := object["objective"]; ok {
		text, isString := objective.(string)
		if !isString || utf8.RuneCountInString(text) > MaxObjectiveRunes {
			return nil, fmt.Errorf("argument %q must be a string of at most %d characters", "objective", MaxObjectiveRunes)
		}
	}
	return json.Marshal(object)
}

func validateWorkflowRunIDArgs(raw json.RawMessage) (json.RawMessage, error) {
	object, err := decodeToolObject(raw)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownToolKeys(object, "run_id"); err != nil {
		return nil, err
	}
	if err := requireToolString(object, "run_id"); err != nil {
		return nil, err
	}
	return json.Marshal(object)
}

func validateWorkflowProposeArgs(raw json.RawMessage) (json.RawMessage, error) {
	object, err := decodeToolObject(raw)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownToolKeys(object, "spec"); err != nil {
		return nil, err
	}
	if spec, ok := object["spec"].(map[string]any); !ok || len(spec) == 0 {
		return nil, fmt.Errorf("argument %q must be a workflow spec object", "spec")
	}
	return json.Marshal(object)
}

func validateWorkflowSaveRoutineArgs(raw json.RawMessage) (json.RawMessage, error) {
	object, err := decodeToolObject(raw)
	if err != nil {
		return nil, err
	}
	if err := rejectUnknownToolKeys(object, "run_id", "name"); err != nil {
		return nil, err
	}
	if err := requireToolString(object, "run_id"); err != nil {
		return nil, err
	}
	if err := requireToolString(object, "name"); err != nil {
		return nil, err
	}
	return json.Marshal(object)
}

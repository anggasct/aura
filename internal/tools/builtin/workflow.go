package toolsbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/toolbroker"
	"github.com/anggasct/aura/internal/tools"
)

const (
	WorkflowCodeNotPermitted = "workflow_not_permitted"
	WorkflowCodeRunNotFound  = "workflow_run_not_found"
)

var (
	ErrWorkflowNotPermitted = errors.New(WorkflowCodeNotPermitted)
	ErrWorkflowRunNotFound  = errors.New(WorkflowCodeRunNotFound)
)

type WorkflowCaller struct {
	SessionID      string
	PrincipalID    string
	PrincipalClass principal.Class
	AgentID        string
}

type WorkflowInfo struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Goal    string `json:"goal"`
}

type WorkflowStepState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type WorkflowRunState struct {
	RunID      string              `json:"run_id"`
	WorkflowID string              `json:"workflow_id,omitempty"`
	Status     string              `json:"status"`
	Steps      []WorkflowStepState `json:"steps,omitempty"`
}

type WorkflowController interface {
	List(ctx context.Context, caller *WorkflowCaller) ([]WorkflowInfo, error)
	Start(ctx context.Context, caller *WorkflowCaller, id, objective string) (WorkflowRunState, error)
	Status(ctx context.Context, caller *WorkflowCaller, runID string) (WorkflowRunState, error)
	Cancel(ctx context.Context, caller *WorkflowCaller, runID string) (WorkflowRunState, error)
}

type workflowHolder struct {
	controller atomic.Pointer[WorkflowController]
}

func (e *Executor) SetWorkflowController(controller WorkflowController) {
	if e == nil || e.workflows == nil || controller == nil {
		return
	}
	e.workflows.controller.Store(&controller)
}

func (h *workflowHolder) adapters() map[string]toolbroker.Adapter {
	return map[string]toolbroker.Adapter{
		tools.WorkflowListToolName + "@" + tools.Version: h.adapter(func(ctx context.Context, controller WorkflowController, caller *WorkflowCaller, _ workflowArgs) (any, error) {
			workflows, err := controller.List(ctx, caller)
			if workflows == nil {
				workflows = []WorkflowInfo{}
			}
			return map[string][]WorkflowInfo{"workflows": workflows}, err
		}),
		tools.WorkflowStartToolName + "@" + tools.Version: h.adapter(func(ctx context.Context, controller WorkflowController, caller *WorkflowCaller, args workflowArgs) (any, error) {
			return controller.Start(ctx, caller, args.ID, args.Objective)
		}),
		tools.WorkflowStatusToolName + "@" + tools.Version: h.adapter(func(ctx context.Context, controller WorkflowController, caller *WorkflowCaller, args workflowArgs) (any, error) {
			return controller.Status(ctx, caller, args.RunID)
		}),
		tools.WorkflowCancelToolName + "@" + tools.Version: h.adapter(func(ctx context.Context, controller WorkflowController, caller *WorkflowCaller, args workflowArgs) (any, error) {
			return controller.Cancel(ctx, caller, args.RunID)
		}),
	}
}

type workflowArgs struct {
	ID        string `json:"id"`
	Objective string `json:"objective"`
	RunID     string `json:"run_id"`
}

type workflowCall func(ctx context.Context, controller WorkflowController, caller *WorkflowCaller, args workflowArgs) (any, error)

func (h *workflowHolder) adapter(call workflowCall) toolbroker.Adapter {
	return func(ctx context.Context, request *toolbroker.ToolRequest, _ approval.Constraints) (toolbroker.ToolResult, error) {
		if request == nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "request must not be nil")
		}
		controller := h.controller.Load()
		if controller == nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultCapabilityUnavailable, "workflows need the durable runtime")
		}
		var args workflowArgs
		if err := json.Unmarshal(request.Arguments, &args); err != nil {
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "arguments must be a JSON object")
		}
		class := request.PrincipalClass
		if class == "" {
			class = principal.ClassOwner
		}
		result, err := call(ctx, *controller, &WorkflowCaller{
			SessionID: request.SessionID, PrincipalID: request.PrincipalID, PrincipalClass: class, AgentID: request.AgentID,
		}, args)
		switch {
		case errors.Is(err, ErrWorkflowNotPermitted):
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultPolicyDenied, "%s: this workflow is not available here", WorkflowCodeNotPermitted)
		case errors.Is(err, ErrWorkflowRunNotFound):
			return toolbroker.ToolResult{}, toolbroker.Errorf(toolbroker.ResultInvalidArgument, "%s: no such workflow run", WorkflowCodeRunNotFound)
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

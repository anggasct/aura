package workflow

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
)

type AgentResolver interface {
	Resolve(required []string, preferID *string) (auraagent.Definition, error)
}

type ValidationDeps struct {
	KnownTools         []string
	EffectfulTools     []string
	Agents             AgentResolver
	DefaultStepTimeout time.Duration
	CheckToolCall      func(toolID string, args json.RawMessage) error
}

const MaxCompositionDepth = 3

func Validate(spec *Spec, deps *ValidationDeps) error {
	if spec == nil {
		return codedError(ErrorCodeSpecInvalid, "spec must not be nil")
	}
	if err := checkID("id", spec.ID); err != nil {
		return err
	}
	if err := checkGoal(spec.Goal); err != nil {
		return err
	}
	if spec.Version <= 0 {
		return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("version %d must be positive", spec.Version))
	}
	if !spec.Source.valid() {
		return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("source %q must be defined, composed, or generated", spec.Source))
	}
	if len(spec.Steps) == 0 {
		return codedError(ErrorCodeSpecInvalid, "at least one step is required")
	}

	ids := make(map[string]bool, len(spec.Steps))
	stepsByID := make(map[string]*StepSpec, len(spec.Steps))
	for index := range spec.Steps {
		step := &spec.Steps[index]
		if strings.TrimSpace(step.ID) == "" {
			return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("steps[%d].id must not be empty", index))
		}
		if ids[step.ID] {
			return codedError(ErrorCodeDuplicateStep, fmt.Sprintf("steps[%d].id %q is duplicated", index, step.ID))
		}
		if err := checkID(fmt.Sprintf("steps[%d].id", index), step.ID); err != nil {
			return err
		}
		ids[step.ID] = true
		stepsByID[step.ID] = step
		for _, dependency := range step.DependsOn {
			if dependency == step.ID {
				return codedError(ErrorCodeUnknownDependency, fmt.Sprintf("step %q depends on itself", step.ID))
			}
		}
	}

	for index := range spec.Steps {
		step := &spec.Steps[index]
		for _, dependency := range step.DependsOn {
			if err := checkID(fmt.Sprintf("steps[%d].depends_on entry", index), dependency); err != nil {
				return err
			}
			if !ids[dependency] {
				return codedError(ErrorCodeUnknownDependency, fmt.Sprintf("steps[%d].depends_on references unknown step %q", index, dependency))
			}
		}
	}
	if err := detectCycle(spec); err != nil {
		return err
	}

	for index := range spec.Steps {
		if err := validateExecutor(&spec.Steps[index], deps, ids); err != nil {
			return err
		}
	}

	for index := range spec.Steps {
		step := &spec.Steps[index]
		if step.Condition == nil {
			continue
		}
		if err := checkDisplayText(fmt.Sprintf("steps[%d].condition", index), *step.Condition, maxConditionRunes); err != nil {
			return err
		}
		parsed, err := parseCondition(*step.Condition)
		if err != nil {
			return err
		}
		for _, referenced := range parsed.referencedSteps() {
			if !ids[referenced] {
				return codedError(ErrorCodeConditionInvalid, fmt.Sprintf("steps[%d].condition references unknown step %q", index, referenced))
			}
		}
	}

	for index := range spec.Steps {
		step := &spec.Steps[index]
		if step.Timeout <= 0 && deps.DefaultStepTimeout > 0 {
			step.Timeout = deps.DefaultStepTimeout
		}
		if step.Timeout <= 0 {
			return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("steps[%d].timeout must be positive", index))
		}
		if step.Retry.Attempts < 0 || step.Retry.Attempts > maxRetryAttempts {
			return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("steps[%d].retry.attempts %d must be in [0, %d]", index, step.Retry.Attempts, maxRetryAttempts))
		}
		if step.Retry.Backoff < 0 || step.Retry.Backoff > maxRetryBackoff {
			return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("steps[%d].retry.backoff %s must be in [0, %s]", index, step.Retry.Backoff, maxRetryBackoff))
		}
	}

	for index := range spec.Steps {
		step := &spec.Steps[index]
		if step.Executor.Kind != KindTool || step.Executor.ToolID == nil {
			continue
		}
		if !slices.Contains(deps.EffectfulTools, *step.Executor.ToolID) {
			continue
		}
		if hasTransitiveApproval(stepsByID, step) {
			continue
		}
		return codedError(ErrorCodeDangerousUncovered, fmt.Sprintf("steps[%d] uses effectful tool %q without an approval step among its transitive predecessors", index, *step.Executor.ToolID))
	}
	return nil
}

func validateExecutor(step *StepSpec, deps *ValidationDeps, ids map[string]bool) error {
	field := fmt.Sprintf("step %q executor", step.ID)
	switch step.Executor.Kind {
	case KindAgent:
		hasAgent := step.Executor.AgentID != nil && *step.Executor.AgentID != ""
		if !hasAgent && len(step.Executor.RequiredCapabilities) == 0 {
			return codedError(ErrorCodeExecutorInvalid, field+" needs agent_id or a non-empty requires list")
		}
		if hasAgent {
			if err := checkDisplaySafe(field+" agent_id", *step.Executor.AgentID); err != nil {
				return err
			}
		}
		for _, capability := range step.Executor.RequiredCapabilities {
			if err := checkDisplaySafe(field+" requires entry", capability); err != nil {
				return err
			}
		}
		if hasAgent && deps.Agents != nil {
			agentID := *step.Executor.AgentID
			if _, err := deps.Agents.Resolve(nil, &agentID); err != nil {
				return codedError(ErrorCodeExecutorInvalid, field+" references unknown agent "+fmt.Sprintf("%q", agentID))
			}
		}
		if deps.Agents != nil && len(step.Executor.RequiredCapabilities) > 0 {
			if _, err := deps.Agents.Resolve(step.Executor.RequiredCapabilities, nil); err != nil {
				return codedError(ErrorCodeExecutorInvalid, field+" requirements are not resolvable: "+err.Error())
			}
		}
	case KindTool:
		if step.Executor.ToolID == nil || *step.Executor.ToolID == "" {
			return codedError(ErrorCodeExecutorInvalid, field+" needs tool_id")
		}
		if !slices.Contains(deps.KnownTools, *step.Executor.ToolID) {
			return codedError(ErrorCodeExecutorInvalid, field+" references unknown tool "+fmt.Sprintf("%q", *step.Executor.ToolID))
		}
		if len(step.Executor.ToolArgs) != 0 {
			var document map[string]json.RawMessage
			if err := json.Unmarshal(step.Executor.ToolArgs, &document); err != nil || document == nil {
				return codedError(ErrorCodeExecutorInvalid, field+" args must be a JSON object")
			}
		}
		if deps.CheckToolCall == nil {
			return codedError(ErrorCodeSpecInvalid, field+" cannot be checked: no tool argument checker is configured")
		}
		if err := deps.CheckToolCall(*step.Executor.ToolID, toolArguments(step)); err != nil {
			return codedError(ErrorCodeSpecInvalid, field+" arguments are invalid: "+err.Error())
		}
	case KindWait:
		if step.Executor.Event == nil || *step.Executor.Event == "" {
			return codedError(ErrorCodeExecutorInvalid, field+" needs event")
		}
		if err := checkDisplayText(field+" event", *step.Executor.Event, maxEventRunes); err != nil {
			return err
		}
		if step.Executor.Source != nil && *step.Executor.Source == "" {
			return codedError(ErrorCodeExecutorInvalid, field+" source must not be empty")
		}
		if step.Executor.ExternalRef != nil {
			if *step.Executor.ExternalRef == "" {
				return codedError(ErrorCodeExecutorInvalid, field+" external_ref must not be empty")
			}
			if err := checkID(field+" external_ref", *step.Executor.ExternalRef); err != nil {
				return err
			}
			if !ids[*step.Executor.ExternalRef] {
				return codedError(ErrorCodeExecutorInvalid, field+" external_ref references unknown step "+fmt.Sprintf("%q", *step.Executor.ExternalRef))
			}
			if !slices.Contains(step.DependsOn, *step.Executor.ExternalRef) {
				return codedError(ErrorCodeExecutorInvalid, field+" external_ref must reference a dependency")
			}
		}
	case KindApproval:
		if step.Executor.AgentID != nil || len(step.Executor.RequiredCapabilities) > 0 || step.Executor.ToolID != nil || step.Executor.Event != nil || step.Executor.WorkflowID != nil {
			return codedError(ErrorCodeExecutorInvalid, field+" takes no executor extras")
		}
	case KindWorkflow:
		if step.Executor.WorkflowID == nil || *step.Executor.WorkflowID == "" {
			return codedError(ErrorCodeExecutorInvalid, field+" needs workflow")
		}
		if err := checkID(field+" workflow", *step.Executor.WorkflowID); err != nil {
			return err
		}
		if step.Executor.AgentID != nil || len(step.Executor.RequiredCapabilities) > 0 || step.Executor.ToolID != nil || step.Executor.Event != nil {
			return codedError(ErrorCodeExecutorInvalid, field+" takes only workflow")
		}
	default:
		return codedError(ErrorCodeExecutorInvalid, fmt.Sprintf("%s kind %q is not agent, tool, wait, approval, or workflow", field, step.Executor.Kind))
	}
	return nil
}

func detectCycle(spec *Spec) error {
	state := make(map[string]int, len(spec.Steps))
	byID := make(map[string]*StepSpec, len(spec.Steps))
	for index := range spec.Steps {
		byID[spec.Steps[index].ID] = &spec.Steps[index]
	}
	var visit func(stepID string, path []string) error
	visit = func(stepID string, path []string) error {
		switch state[stepID] {
		case 1:
			start := slices.Index(path, stepID)
			cycle := append(slices.Clone(path[start:]), stepID)
			return codedError(ErrorCodeCycleDetected, "dependency cycle: "+strings.Join(cycle, " -> "))
		case 2:
			return nil
		}
		state[stepID] = 1
		for _, dependency := range byID[stepID].DependsOn {
			if err := visit(dependency, append(path, stepID)); err != nil {
				return err
			}
		}
		state[stepID] = 2
		return nil
	}
	for index := range spec.Steps {
		if err := visit(spec.Steps[index].ID, nil); err != nil {
			return err
		}
	}
	return nil
}

func hasTransitiveApproval(stepsByID map[string]*StepSpec, step *StepSpec) bool {
	seen := map[string]bool{}
	stack := append([]string(nil), step.DependsOn...)
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[current] {
			continue
		}
		seen[current] = true
		predecessor := stepsByID[current]
		if predecessor.Executor.Kind == KindApproval {
			return true
		}
		stack = append(stack, predecessor.DependsOn...)
	}
	return false
}

func ValidateComposition(spec *Spec, lookup func(id string) (*Spec, bool)) error {
	var depthOf func(current *Spec, path []string) (int, error)
	depthOf = func(current *Spec, path []string) (int, error) {
		if slices.Contains(path, current.ID) {
			return 0, codedError(ErrorCodeCompositionInvalid, fmt.Sprintf("workflow %q reaches itself through %s", spec.ID, strings.Join(append(path, current.ID), " -> ")))
		}
		path = append(path, current.ID)
		deepest := 0
		for index := range current.Steps {
			step := &current.Steps[index]
			if step.Executor.Kind != KindWorkflow || step.Executor.WorkflowID == nil {
				continue
			}
			childID := *step.Executor.WorkflowID
			if lookup == nil {
				return 0, codedError(ErrorCodeCompositionInvalid, fmt.Sprintf("step %q references workflow %q, but no workflow set is loaded", step.ID, childID))
			}
			child, ok := lookup(childID)
			if !ok {
				return 0, codedError(ErrorCodeCompositionInvalid, fmt.Sprintf("step %q references unknown workflow %q", step.ID, childID))
			}
			depth, err := depthOf(child, path)
			if err != nil {
				return 0, err
			}
			deepest = max(deepest, depth)
		}
		return deepest + 1, nil
	}
	depth, err := depthOf(spec, nil)
	if err != nil {
		return err
	}
	if depth > MaxCompositionDepth {
		return codedError(ErrorCodeCompositionInvalid, fmt.Sprintf("workflow %q composes %d levels deep, more than %d", spec.ID, depth, MaxCompositionDepth))
	}
	return nil
}

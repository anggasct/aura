package workflow

import (
	"fmt"
	"slices"
	"strings"
	"time"

	auraagent "github.com/anggasct/aura/internal/agent"
)

// AgentResolver is the registry surface validation dry-runs against; the
// interface is declared here so validation depends only on resolution.
type AgentResolver interface {
	Resolve(required []string, preferID *string) (auraagent.Definition, error)
}

// ValidationDeps carries the registries validation checks references
// against; the composition root supplies them.
type ValidationDeps struct {
	// KnownTools lists registered tool names.
	KnownTools []string
	// EffectfulTools lists tool names that require approval coverage.
	EffectfulTools []string
	// Agents resolves agent requirements (dry-run).
	Agents AgentResolver
	// DefaultStepTimeout fills an omitted per-step timeout before the
	// presence check.
	DefaultStepTimeout time.Duration
}

// Validate applies the frozen validation rules in order; the first failing
// rule returns its exact field-level error.
func Validate(spec *Spec, deps ValidationDeps) error {
	if spec == nil {
		return codedError(ErrorCodeSpecInvalid, "spec must not be nil")
	}
	if strings.TrimSpace(spec.ID) == "" {
		return codedError(ErrorCodeSpecInvalid, "id must not be empty")
	}
	if strings.TrimSpace(spec.Goal) == "" {
		return codedError(ErrorCodeSpecInvalid, "goal must not be empty")
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
			if !ids[dependency] {
				return codedError(ErrorCodeUnknownDependency, fmt.Sprintf("steps[%d].depends_on references unknown step %q", index, dependency))
			}
		}
	}
	if err := detectCycle(spec); err != nil {
		return err
	}

	for index := range spec.Steps {
		if err := validateExecutor(&spec.Steps[index], deps); err != nil {
			return err
		}
	}

	for index := range spec.Steps {
		step := &spec.Steps[index]
		if step.Condition == nil {
			continue
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

func validateExecutor(step *StepSpec, deps ValidationDeps) error {
	field := fmt.Sprintf("step %q executor", step.ID)
	switch step.Executor.Kind {
	case KindAgent:
		hasAgent := step.Executor.AgentID != nil && *step.Executor.AgentID != ""
		if !hasAgent && len(step.Executor.RequiredCapabilities) == 0 {
			return codedError(ErrorCodeExecutorInvalid, field+" needs agent_id or a non-empty requires list")
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
	case KindWait:
		if step.Executor.Event == nil || *step.Executor.Event == "" {
			return codedError(ErrorCodeExecutorInvalid, field+" needs event")
		}
	case KindApproval:
		if step.Executor.AgentID != nil || len(step.Executor.RequiredCapabilities) > 0 || step.Executor.ToolID != nil || step.Executor.Event != nil {
			return codedError(ErrorCodeExecutorInvalid, field+" takes no executor extras")
		}
	default:
		return codedError(ErrorCodeExecutorInvalid, fmt.Sprintf("%s kind %q is not agent, tool, wait, or approval", field, step.Executor.Kind))
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

// hasTransitiveApproval reports whether an approval step exists among the
// step's transitive predecessors.
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

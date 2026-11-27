package workflow

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	auraagent "github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
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
	StepAllowed        func(agentID string, class principal.Class, toolID string) (allowed, personaKnown bool)
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
		if err := validateExecutor(&spec.Steps[index], deps, stepsByID); err != nil {
			return err
		}
	}

	conditions := make(map[string]*condition, len(spec.Steps))
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
		conditions[step.ID] = parsed
	}
	if err := checkWebStepInputs(spec, conditions); err != nil {
		return err
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

func validateExecutor(step *StepSpec, deps *ValidationDeps, stepsByID map[string]*StepSpec) error {
	field := fmt.Sprintf("step %q executor", step.ID)
	switch step.Executor.Kind {
	case KindAgent:
		return validateAgentExecutor(step, deps, field)
	case KindTool:
		if step.Executor.hasAgentFields() {
			return codedError(ErrorCodeExecutorInvalid, field+" takes no agent fields")
		}
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
		if step.Executor.hasAgentFields() {
			return codedError(ErrorCodeExecutorInvalid, field+" takes no agent fields")
		}
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
			referenced, known := stepsByID[*step.Executor.ExternalRef]
			if !known {
				return codedError(ErrorCodeExecutorInvalid, field+" external_ref references unknown step "+fmt.Sprintf("%q", *step.Executor.ExternalRef))
			}
			if !slices.Contains(step.DependsOn, *step.Executor.ExternalRef) {
				return codedError(ErrorCodeExecutorInvalid, field+" external_ref must reference a dependency")
			}
			if referenced.Executor.Kind == KindAgent {
				return codedError(ErrorCodeExecutorInvalid, field+" external_ref must not name an agent step")
			}
		}
	case KindApproval:
		if step.Executor.hasAgentFields() || step.Executor.ToolID != nil || step.Executor.Event != nil || step.Executor.WorkflowID != nil {
			return codedError(ErrorCodeExecutorInvalid, field+" takes no executor extras")
		}
	case KindWorkflow:
		if step.Executor.WorkflowID == nil || *step.Executor.WorkflowID == "" {
			return codedError(ErrorCodeExecutorInvalid, field+" needs workflow")
		}
		if err := checkID(field+" workflow", *step.Executor.WorkflowID); err != nil {
			return err
		}
		if step.Executor.hasAgentFields() || step.Executor.ToolID != nil || step.Executor.Event != nil {
			return codedError(ErrorCodeExecutorInvalid, field+" takes only workflow")
		}
	default:
		return codedError(ErrorCodeExecutorInvalid, fmt.Sprintf("%s kind %q is not agent, tool, wait, approval, or workflow", field, step.Executor.Kind))
	}
	return nil
}

const (
	toolReadFile  = "read_file"
	toolListDir   = "list_dir"
	toolWebSearch = "web_search"
	toolWebFetch  = "web_fetch"

	maxAgentPromptRunes = 2000
)

func isWorkspaceTool(toolID string) bool {
	return toolID == toolReadFile || toolID == toolListDir
}

func isWebTool(toolID string) bool {
	return toolID == toolWebSearch || toolID == toolWebFetch
}

func hasWebTool(step *StepSpec) bool {
	return step.Executor.Kind == KindAgent && slices.ContainsFunc(step.Executor.Tools, isWebTool)
}

func hasWorkspaceTool(step *StepSpec) bool {
	return step.Executor.Kind == KindAgent && slices.ContainsFunc(step.Executor.Tools, isWorkspaceTool)
}

func validateAgentExecutor(step *StepSpec, deps *ValidationDeps, field string) error {
	executor := &step.Executor
	if executor.ToolID != nil || len(executor.ToolArgs) != 0 || executor.Event != nil || executor.Source != nil || executor.ExternalRef != nil || executor.WorkflowID != nil {
		return codedError(ErrorCodeExecutorInvalid, field+" takes no tool, args, event, source, external_ref or workflow")
	}
	if executor.AgentID == nil || *executor.AgentID == "" {
		return codedError(ErrorCodeSpecInvalid, field+" needs agent_id")
	}
	agentID := *executor.AgentID
	if err := checkDisplaySafe(field+" agent_id", agentID); err != nil {
		return err
	}
	if deps.Agents != nil {
		if _, err := deps.Agents.Resolve(nil, &agentID); err != nil {
			return codedError(ErrorCodeExecutorInvalid, field+" references unknown agent "+fmt.Sprintf("%q", agentID))
		}
	}
	if err := checkAgentPrompt(field+" prompt", executor.Prompt); err != nil {
		return err
	}
	switch executor.Output {
	case "", OutputText, OutputJSON:
	default:
		return codedError(ErrorCodeExecutorInvalid, field+" output must be text or json")
	}
	executor.applyAgentDefaults()
	if *executor.MaxTokens < 1 || *executor.MaxTokens > MaxAgentMaxTokens {
		return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("%s max_tokens %d must be in [1, %d]", field, *executor.MaxTokens, MaxAgentMaxTokens))
	}
	return checkAgentTools(field, executor.Tools)
}

func checkAgentPrompt(field, prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return codedError(ErrorCodeSpecInvalid, field+" must not be empty")
	}
	if utf8.RuneCountInString(prompt) > maxAgentPromptRunes {
		return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("%s is longer than %d characters", field, maxAgentPromptRunes))
	}
	return checkDisplaySafe(field, strings.ReplaceAll(prompt, "\n", ""))
}

func checkAgentTools(field string, declared []string) error {
	for _, toolID := range declared {
		if !isWorkspaceTool(toolID) && !isWebTool(toolID) {
			return codedError(ErrorCodeExecutorInvalid, fmt.Sprintf("%s tools entry %q is not read_file, list_dir, web_search or web_fetch", field, tools.EscapeDisplayText(toolID)))
		}
	}
	if slices.ContainsFunc(declared, isWorkspaceTool) && slices.ContainsFunc(declared, isWebTool) {
		return codedError(ErrorCodeExecutorInvalid, field+" tools may name workspace tools or web tools, never both")
	}
	return nil
}

type stepLabels struct {
	private    map[string]bool
	influenced map[string]bool
}

func computeStepLabels(spec *Spec, conditions map[string]*condition) *stepLabels {
	labels := &stepLabels{private: make(map[string]bool, len(spec.Steps)), influenced: make(map[string]bool, len(spec.Steps))}
	for changed := true; changed; {
		changed = false
		for index := range spec.Steps {
			step := &spec.Steps[index]
			if !labels.private[step.ID] && labels.isPrivate(step) {
				labels.private[step.ID] = true
				changed = true
			}
			if !labels.influenced[step.ID] && labels.isInfluenced(step, conditions[step.ID]) {
				labels.influenced[step.ID] = true
				changed = true
			}
		}
	}
	return labels
}

func (l *stepLabels) isPrivate(step *StepSpec) bool {
	switch step.Executor.Kind {
	case KindTool:
		return !isWebTool(deref(step.Executor.ToolID))
	case KindAgent:
		return hasWorkspaceTool(step) || slices.ContainsFunc(step.DependsOn, func(dependency string) bool { return l.private[dependency] })
	default:
		return false
	}
}

func (l *stepLabels) isInfluenced(step *StepSpec, parsed *condition) bool {
	if slices.ContainsFunc(step.DependsOn, func(dependency string) bool { return l.influenced[dependency] }) {
		return true
	}
	if parsed == nil {
		return false
	}
	return slices.ContainsFunc(parsed.references(), func(ref *refOperand) bool {
		return l.influenced[ref.stepID] || (ref.field == "output" && l.private[ref.stepID])
	})
}

func (c *condition) references() []*refOperand {
	var refs []*refOperand
	for index := range c.comparisons {
		for _, side := range []*operand{&c.comparisons[index].left, &c.comparisons[index].right} {
			if side.ref != nil {
				refs = append(refs, side.ref)
			}
		}
	}
	return refs
}

func checkWebStepInputs(spec *Spec, conditions map[string]*condition) error {
	hasWeb := false
	for index := range spec.Steps {
		hasWeb = hasWeb || hasWebTool(&spec.Steps[index])
	}
	if !hasWeb {
		return nil
	}
	labels := computeStepLabels(spec, conditions)
	for index := range spec.Steps {
		step := &spec.Steps[index]
		if !hasWebTool(step) {
			continue
		}
		field := fmt.Sprintf("step %q executor", step.ID)
		for _, dependency := range step.DependsOn {
			if labels.private[dependency] {
				return codedError(ErrorCodeExecutorInvalid, fmt.Sprintf("%s uses web tools and cannot depend on private step %q", field, dependency))
			}
		}
		if conditions[step.ID] != nil {
			for _, ref := range conditions[step.ID].references() {
				if labels.private[ref.stepID] {
					return codedError(ErrorCodeExecutorInvalid, fmt.Sprintf("%s uses web tools and its condition cannot read private step %q", field, ref.stepID))
				}
			}
		}
		if labels.influenced[step.ID] {
			return codedError(ErrorCodeExecutorInvalid, field+" uses web tools and cannot be influenced by a private step through its dependencies or condition")
		}
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

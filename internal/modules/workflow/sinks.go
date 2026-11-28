package workflow

import (
	"fmt"
	"slices"
)

func CheckUnattendedSinks(spec *Spec, deps *ValidationDeps) error {
	if spec == nil || deps == nil {
		return codedError(ErrorCodeInvalidArgument, "the unattended sink check needs a spec and its tool sets")
	}
	conditions, err := parsedConditions(spec)
	if err != nil {
		return err
	}
	downstream := downstreamOfWebSteps(spec, deps, conditions)
	for index := range spec.Steps {
		step := &spec.Steps[index]
		if downstream[step.ID] && !sinkFree(step, deps) {
			return codedError(ErrorCodeUnattendedSink, fmt.Sprintf("step %q follows a web step and could send out; a scheduled workflow keeps only agent steps without web tools, tool steps that are neither sinks nor effectful, wait steps and approval steps after one", step.ID))
		}
	}
	return nil
}

func parsedConditions(spec *Spec) (map[string]*condition, error) {
	conditions := make(map[string]*condition, len(spec.Steps))
	for index := range spec.Steps {
		step := &spec.Steps[index]
		if step.Condition == nil {
			continue
		}
		parsed, err := parseCondition(*step.Condition)
		if err != nil {
			return nil, err
		}
		conditions[step.ID] = parsed
	}
	return conditions, nil
}

func isWebStep(step *StepSpec, deps *ValidationDeps) bool {
	switch step.Executor.Kind {
	case KindAgent:
		return hasWebTool(step)
	case KindTool:
		return slices.Contains(deps.SinkTools, deref(step.Executor.ToolID))
	case KindWait, KindApproval, KindWorkflow:
		return false
	}
	return false
}

func downstreamOfWebSteps(spec *Spec, deps *ValidationDeps, conditions map[string]*condition) map[string]bool {
	web := make(map[string]bool, len(spec.Steps))
	for index := range spec.Steps {
		if isWebStep(&spec.Steps[index], deps) {
			web[spec.Steps[index].ID] = true
		}
	}
	downstream := make(map[string]bool, len(spec.Steps))
	for changed := true; changed; {
		changed = false
		for index := range spec.Steps {
			step := &spec.Steps[index]
			if downstream[step.ID] || !followsAny(step, conditions[step.ID], func(id string) bool { return web[id] || downstream[id] }) {
				continue
			}
			downstream[step.ID] = true
			changed = true
		}
	}
	return downstream
}

func followsAny(step *StepSpec, parsed *condition, marked func(id string) bool) bool {
	if slices.ContainsFunc(step.DependsOn, marked) {
		return true
	}
	if parsed == nil {
		return false
	}
	return slices.ContainsFunc(parsed.referencedSteps(), marked)
}

func sinkFree(step *StepSpec, deps *ValidationDeps) bool {
	switch step.Executor.Kind {
	case KindAgent:
		return !hasWebTool(step)
	case KindTool:
		tool := deref(step.Executor.ToolID)
		return !slices.Contains(deps.SinkTools, tool) && !slices.Contains(deps.EffectfulTools, tool)
	case KindWait, KindApproval:
		return true
	case KindWorkflow:
		return false
	}
	return false
}

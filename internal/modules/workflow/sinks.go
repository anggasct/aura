package workflow

import (
	"fmt"
	"slices"
)

type ChildLookup func(id string) (*Spec, error)

type sinkCheck struct {
	deps     *ValidationDeps
	lookup   ChildLookup
	checking map[string]bool
	walking  map[string]bool
	hasWeb   map[string]bool
}

func CheckUnattendedSinks(spec *Spec, deps *ValidationDeps, lookup ChildLookup) error {
	if spec == nil || deps == nil {
		return codedError(ErrorCodeInvalidArgument, "the unattended sink check needs a spec and its tool sets")
	}
	check := &sinkCheck{deps: deps, lookup: lookup, checking: map[string]bool{}, walking: map[string]bool{}, hasWeb: map[string]bool{}}
	return check.check(spec.ID, spec)
}

func (c *sinkCheck) refuse(format string, args ...any) error {
	return codedError(ErrorCodeUnattendedSink, fmt.Sprintf(format, args...))
}

func (c *sinkCheck) child(step *StepSpec) (id string, spec *Spec, err error) {
	id = deref(step.Executor.WorkflowID)
	if c.lookup == nil {
		return id, nil, c.refuse("step %q starts workflow %q, which cannot be read to check it", step.ID, id)
	}
	spec, err = c.lookup(id)
	if err != nil || spec == nil {
		return id, nil, c.refuse("step %q starts workflow %q, which cannot be read to check it", step.ID, id)
	}
	return id, spec, nil
}

func (c *sinkCheck) check(id string, spec *Spec) error {
	if c.checking[id] {
		return c.refuse("workflow %q starts itself, directly or through another workflow", id)
	}
	c.checking[id] = true
	defer delete(c.checking, id)
	conditions, err := parsedConditions(spec)
	if err != nil {
		return err
	}
	web := make(map[string]bool, len(spec.Steps))
	for index := range spec.Steps {
		step := &spec.Steps[index]
		if step.Executor.Kind == KindWorkflow {
			childID, childSpec, err := c.child(step)
			if err != nil {
				return err
			}
			if err := c.check(childID, childSpec); err != nil {
				return err
			}
		}
		isWeb, err := c.webStep(step)
		if err != nil {
			return err
		}
		web[step.ID] = isWeb
	}
	downstream := downstreamOfWebSteps(spec, web, conditions)
	for index := range spec.Steps {
		step := &spec.Steps[index]
		if !downstream[step.ID] {
			continue
		}
		free, err := c.sinkFree(step)
		if err != nil {
			return err
		}
		if !free {
			return c.refuse("step %q follows a web step and could send out; a scheduled workflow keeps only agent steps without web tools, tool steps that are neither sinks nor effectful, wait steps and approval steps after one", step.ID)
		}
	}
	return nil
}

func (c *sinkCheck) webStep(step *StepSpec) (bool, error) {
	switch step.Executor.Kind {
	case KindAgent:
		return hasWebTool(step), nil
	case KindTool:
		return slices.Contains(c.deps.SinkTools, deref(step.Executor.ToolID)), nil
	case KindWorkflow:
		id, spec, err := c.child(step)
		if err != nil {
			return false, err
		}
		return c.containsWeb(id, spec)
	case KindWait, KindApproval:
		return false, nil
	}
	return false, nil
}

func (c *sinkCheck) containsWeb(id string, spec *Spec) (bool, error) {
	if known, done := c.hasWeb[id]; done {
		return known, nil
	}
	if c.walking[id] {
		return false, c.refuse("workflow %q starts itself, directly or through another workflow", id)
	}
	c.walking[id] = true
	defer delete(c.walking, id)
	found := false
	for index := range spec.Steps {
		isWeb, err := c.webStep(&spec.Steps[index])
		if err != nil {
			return false, err
		}
		found = found || isWeb
	}
	c.hasWeb[id] = found
	return found, nil
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

func downstreamOfWebSteps(spec *Spec, web map[string]bool, conditions map[string]*condition) map[string]bool {
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

func (c *sinkCheck) sinkFree(step *StepSpec) (bool, error) {
	switch step.Executor.Kind {
	case KindAgent:
		return !hasWebTool(step), nil
	case KindTool:
		tool := deref(step.Executor.ToolID)
		return !slices.Contains(c.deps.SinkTools, tool) && !slices.Contains(c.deps.EffectfulTools, tool), nil
	case KindWait, KindApproval:
		return true, nil
	case KindWorkflow:
		id, spec, err := c.child(step)
		if err != nil {
			return false, err
		}
		return c.freeOfSinks(id, spec)
	}
	return false, nil
}

func (c *sinkCheck) freeOfSinks(id string, spec *Spec) (bool, error) {
	if c.walking[id] {
		return false, c.refuse("workflow %q starts itself, directly or through another workflow", id)
	}
	c.walking[id] = true
	defer delete(c.walking, id)
	for index := range spec.Steps {
		free, err := c.sinkFree(&spec.Steps[index])
		if err != nil || !free {
			return false, err
		}
	}
	return true, nil
}

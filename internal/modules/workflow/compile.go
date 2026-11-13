package workflow

import (
	"fmt"
	"slices"

	"github.com/anggasct/aura/internal/platform/principal"
)

type Graph struct {
	Order      []string
	ByStep     map[string]*StepSpec
	Deps       map[string][]string
	Dependents map[string][]string
}

func Compile(spec *Spec, deps *ValidationDeps) (*Graph, error) {
	if err := Validate(spec, deps); err != nil {
		return nil, err
	}
	if err := checkRunAs(spec, deps); err != nil {
		return nil, err
	}
	byStep := make(map[string]*StepSpec, len(spec.Steps))
	dependents := make(map[string][]string, len(spec.Steps))
	indegree := make(map[string]int, len(spec.Steps))
	for index := range spec.Steps {
		step := &spec.Steps[index]
		byStep[step.ID] = step
		indegree[step.ID] = len(step.DependsOn)
		for _, dependency := range step.DependsOn {
			dependents[dependency] = append(dependents[dependency], step.ID)
		}
	}
	depsOf := make(map[string][]string, len(spec.Steps))
	for index := range spec.Steps {
		step := &spec.Steps[index]
		depsOf[step.ID] = append([]string(nil), step.DependsOn...)
	}
	var order []string
	var frontier []string
	for index := range spec.Steps {
		if indegree[spec.Steps[index].ID] == 0 {
			frontier = append(frontier, spec.Steps[index].ID)
		}
	}
	slices.Sort(frontier)
	for len(frontier) > 0 {
		current := frontier[0]
		frontier = frontier[1:]
		order = append(order, current)
		for _, dependent := range sortedCopy(dependents[current]) {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				frontier = append(frontier, dependent)
				slices.Sort(frontier)
			}
		}
	}
	if len(order) != len(spec.Steps) {
		return nil, codedError(ErrorCodeCycleDetected, "dependency graph is cyclic")
	}
	return &Graph{Order: order, ByStep: byStep, Deps: depsOf, Dependents: dependents}, nil
}

func sortedCopy(values []string) []string {
	result := append([]string(nil), values...)
	slices.Sort(result)
	return result
}

func checkRunAs(spec *Spec, deps *ValidationDeps) error {
	if spec.RunAs == "" {
		return nil
	}
	if err := checkDisplaySafe("run_as", spec.RunAs); err != nil {
		return err
	}
	if deps.Agents == nil {
		return codedError(ErrorCodeSpecInvalid, "run_as cannot be checked: no persona resolver is configured")
	}
	if _, err := deps.Agents.Resolve(nil, &spec.RunAs); err != nil {
		return codedError(ErrorCodeSpecInvalid, fmt.Sprintf("run_as references unknown persona %q", spec.RunAs))
	}
	return CheckCeiling(spec, spec.RunAs, principal.ClassOwner, deps)
}

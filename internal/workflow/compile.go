package workflow

import "slices"

// Graph is the compiled execution graph: a topological step order with
// sorted adjacency for deterministic replay.
type Graph struct {
	Order      []string
	ByStep     map[string]*StepSpec
	Deps       map[string][]string
	Dependents map[string][]string
}

// Compile validates-then-orders the spec into the execution graph.
// Validation always precedes compilation: an invalid spec never yields a
// graph.
func Compile(spec *Spec, deps ValidationDeps) (*Graph, error) {
	if err := Validate(spec, deps); err != nil {
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

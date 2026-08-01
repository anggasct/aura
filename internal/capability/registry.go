package capability

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

type Definition struct {
	Name       string
	Effectful  bool
	Profiles   []Profile
	Dependency string
}

type Dependencies map[string]bool

type State string

const (
	StateUnavailable State = "unavailable"
	StateDisabled    State = "disabled"
	StateEnabled     State = "enabled"
	StateMissing     State = "missing"
)

type Status struct {
	Name              string
	State             State
	Compiled          bool
	Available         bool
	Enabled           bool
	MissingDependency string
}

type Report struct {
	statuses []Status
}

func (r Report) Statuses() []Status {
	return slices.Clone(r.statuses)
}

func (r Report) Status(name string) (Status, bool) {
	for _, status := range r.statuses {
		if status.Name == name {
			return status, true
		}
	}
	return Status{}, false
}

type Registry struct {
	definitions map[string]Definition
}

func EmptyRegistry() Registry {
	return Registry{definitions: map[string]Definition{}}
}

func NewRegistry(definitions ...Definition) (Registry, error) {
	registry := EmptyRegistry()
	for _, definition := range definitions {
		if !namePattern.MatchString(definition.Name) {
			return Registry{}, fmt.Errorf("invalid capability name %q", definition.Name)
		}
		if _, exists := registry.definitions[definition.Name]; exists {
			return Registry{}, fmt.Errorf("duplicate capability definition %q", definition.Name)
		}
		if len(definition.Profiles) == 0 {
			return Registry{}, fmt.Errorf("capability %q has no build profiles", definition.Name)
		}
		profiles := make(map[Profile]struct{}, len(definition.Profiles))
		for _, profile := range definition.Profiles {
			if !profile.Valid() {
				return Registry{}, fmt.Errorf("capability %q has invalid profile %q", definition.Name, profile)
			}
			if definition.Effectful && profile == ProfileCore {
				return Registry{}, fmt.Errorf("effectful capability %q cannot use the core profile", definition.Name)
			}
			if _, exists := profiles[profile]; exists {
				return Registry{}, fmt.Errorf("capability %q repeats profile %q", definition.Name, profile)
			}
			profiles[profile] = struct{}{}
		}
		definition.Profiles = slices.Clone(definition.Profiles)
		registry.definitions[definition.Name] = definition
	}
	return registry, nil
}

func (r Registry) Resolve(build Build, enabled []string, dependencies Dependencies) (Report, error) {
	if !build.Profile().Valid() {
		return Report{}, newError(ErrorCodeProfileInvalid, "", "build profile is invalid")
	}

	enabledSet := make(map[string]struct{}, len(enabled))
	for _, name := range enabled {
		if _, exists := enabledSet[name]; exists {
			return Report{}, newError(ErrorCodeProfileInvalid, name, "duplicate capability")
		}
		enabledSet[name] = struct{}{}
	}
	enabledNames := slices.Clone(enabled)
	slices.Sort(enabledNames)
	for _, name := range enabledNames {
		if _, exists := r.definitions[name]; !exists {
			return Report{}, newError(ErrorCodeCapabilityUnknown, name, "capability is not registered")
		}
	}

	compiledSet := make(map[string]struct{}, len(build.capabilities))
	for _, name := range build.capabilities {
		compiledSet[name] = struct{}{}
	}

	names := make([]string, 0, len(r.definitions))
	for name := range r.definitions {
		names = append(names, name)
	}
	slices.Sort(names)

	statuses := make([]Status, 0, len(names))
	var validationErr error
	for _, name := range names {
		definition := r.definitions[name]
		status := Status{Name: name, State: StateUnavailable}
		_, status.Compiled = compiledSet[name]
		_, status.Enabled = enabledSet[name]
		if !status.Compiled {
			statuses = append(statuses, status)
			continue
		}
		if !slices.Contains(definition.Profiles, build.Profile()) {
			validationErr = errors.Join(validationErr, newError(ErrorCodeCapabilityUnavailable, name, "capability is not included in profile "+string(build.Profile())))
			statuses = append(statuses, status)
			continue
		}
		if definition.Effectful && build.GOOS() != "linux" {
			validationErr = errors.Join(validationErr, newError(ErrorCodeCapabilityUnavailable, name, "effectful capabilities require Linux"))
			statuses = append(statuses, status)
			continue
		}
		if definition.Dependency != "" && !dependencies[definition.Dependency] {
			status.State = StateMissing
			status.MissingDependency = definition.Dependency
			statuses = append(statuses, status)
			if status.Enabled {
				validationErr = errors.Join(validationErr, newError(ErrorCodeDependencyMissing, name, "required dependency "+definition.Dependency+" is missing"))
			}
			continue
		}
		status.Available = true
		if status.Enabled {
			status.State = StateEnabled
		} else {
			status.State = StateDisabled
		}
		statuses = append(statuses, status)
	}

	for _, name := range enabledNames {
		if _, compiled := compiledSet[name]; !compiled {
			validationErr = errors.Join(validationErr, newError(ErrorCodeCapabilityUnavailable, name, "capability is absent from this artifact"))
		}
	}

	return Report{statuses: statuses}, validationErr
}

package workflow

import (
	"context"
	"regexp"
	"slices"
	"strings"
)

var routineName = regexp.MustCompile(`^[A-Za-z0-9 _-]{1,64}$`)

func RoutineID(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if !routineName.MatchString(trimmed) {
		return "", codedError(ErrorCodeRoutineInvalid, "routine name must be 1-64 letters, digits, spaces, '-' or '_'")
	}
	slug := strings.NewReplacer(" ", "_", "-", "_").Replace(strings.ToLower(trimmed))
	return "routine_" + slug, nil
}

func (i *Interpreter) SaveRoutine(ctx context.Context, runID, name string) (*Spec, error) {
	id, err := RoutineID(name)
	if err != nil {
		return nil, err
	}
	run, err := i.store.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.Status != RunSucceeded {
		return nil, codedError(ErrorCodeRoutineInvalid, "run "+runID+" has not succeeded")
	}
	source, err := i.store.Definition(ctx, run.DefinitionID, run.DefinitionVersion)
	if err != nil {
		return nil, err
	}
	if source.Source != SourceGenerated {
		return nil, codedError(ErrorCodeRoutineInvalid, "only runs of generated workflows can be saved as routines")
	}
	version := 1
	if latest, err := i.store.LatestDefinition(ctx, id); err == nil {
		version = latest.Version + 1
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeDefinitionNotFound {
		return nil, err
	}
	routine := &Spec{ID: id, Name: strings.TrimSpace(name), Goal: source.Goal, Version: version, Source: SourceGenerated, Steps: slices.Clone(source.Steps)}
	if err := i.LoadAll(ctx, []*Spec{routine}, i.ValidationDeps()); err != nil {
		return nil, err
	}
	return routine, nil
}

package sqlite

import (
	"fmt"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/platform/fault"
)

type Step struct {
	Version int
	SQL     string
}

type SchemaOwner interface {
	SchemaName() string
	SchemaSQL() string
	SchemaSteps() []Step
}

type StaticSchema struct {
	Name  string
	SQL   string
	Steps []Step
}

func (s StaticSchema) SchemaName() string  { return s.Name }
func (s StaticSchema) SchemaSQL() string   { return s.SQL }
func (s StaticSchema) SchemaSteps() []Step { return s.Steps }

func History(owners ...SchemaOwner) ([]Migration, error) {
	if len(owners) == 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "schema owners must not be empty")
	}
	seen := make(map[string]struct{}, len(owners))
	baselines := make([]Migration, 0, len(owners))
	var steps []Migration
	for i, owner := range owners {
		if owner == nil {
			return nil, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("schema owner %d must not be nil", i))
		}
		name := owner.SchemaName()
		if strings.TrimSpace(name) == "" {
			return nil, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("schema owner %d name must not be empty", i))
		}
		if _, exists := seen[name]; exists {
			return nil, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("duplicate schema owner %q", name))
		}
		seen[name] = struct{}{}
		sqlText := owner.SchemaSQL()
		ownerSteps := owner.SchemaSteps()
		hasBaseline := strings.TrimSpace(sqlText) != ""
		if !hasBaseline && len(ownerSteps) == 0 {
			return nil, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("schema owner %q must declare SQL or steps", name))
		}
		if hasBaseline {
			baselines = append(baselines, Migration{Version: len(baselines) + 1, Owner: name, SQL: sqlText})
		}
		for j, step := range ownerSteps {
			if strings.TrimSpace(step.SQL) == "" {
				return nil, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("schema owner %q step %d SQL must not be empty", name, step.Version))
			}
			if j > 0 && step.Version <= ownerSteps[j-1].Version {
				return nil, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("schema owner %q step %d follows %d, steps must be in increasing order", name, step.Version, ownerSteps[j-1].Version))
			}
			steps = append(steps, Migration{Version: step.Version, Owner: name, SQL: step.SQL})
		}
	}
	slices.SortStableFunc(steps, func(a, b Migration) int { return a.Version - b.Version })
	for i, step := range steps {
		if step.Version <= len(baselines) {
			return nil, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("schema owner %q step %d collides with the %d baselines", step.Owner, step.Version, len(baselines)))
		}
		if i > 0 && step.Version == steps[i-1].Version {
			return nil, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("step version %d is declared by both %q and %q", step.Version, steps[i-1].Owner, step.Owner))
		}
	}
	return append(baselines, steps...), nil
}

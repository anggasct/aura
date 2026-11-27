package sqlite

import (
	"fmt"
	"strings"

	"github.com/anggasct/aura/internal/platform/fault"
)

type SchemaOwner interface {
	SchemaName() string
	SchemaSQL() string
}

type StaticSchema struct {
	Name string
	SQL  string
}

func (s StaticSchema) SchemaName() string { return s.Name }
func (s StaticSchema) SchemaSQL() string  { return s.SQL }

func Baseline(owners ...SchemaOwner) ([]Migration, error) {
	if len(owners) == 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "schema owners must not be empty")
	}
	seen := make(map[string]struct{}, len(owners))
	migrations := make([]Migration, 0, len(owners))
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
		sqlText := owner.SchemaSQL()
		if strings.TrimSpace(sqlText) == "" {
			return nil, fault.New(ErrorCodeInvalidArgument, fmt.Sprintf("schema owner %q SQL must not be empty", name))
		}
		seen[name] = struct{}{}
		migrations = append(migrations, Migration{
			Version: i + 1,
			Owner:   name,
			SQL:     sqlText,
		})
	}
	return migrations, nil
}

package sqlite

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestValidateMigrationOrder(t *testing.T) {
	if err := validateMigrationOrder([]Migration{{Version: 1}, {Version: 2}}); err != nil {
		t.Errorf("strictly increasing list rejected: %v", err)
	}
	for _, ms := range [][]Migration{
		{{Version: 1}, {Version: 1}},
		{{Version: 2}, {Version: 1}},
	} {
		err := validateMigrationOrder(ms)
		if err == nil {
			t.Errorf("validateMigrationOrder(%+v) accepted a non-increasing list", ms)
			continue
		}
		if !fault.HasCode(err, ErrorCodeMigrationSequenceInvalid) {
			t.Errorf("validateMigrationOrder(%+v) = %v, want %s", ms, err, ErrorCodeMigrationSequenceInvalid)
		}
	}
}

func TestMigrateRefusesAnEmptyHistory(t *testing.T) {
	db := openFresh(t, Options{})
	for name, history := range map[string][]Migration{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			if err := Migrate(t.Context(), db, history); !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Errorf("Migrate = %v, want %s", err, ErrorCodeInvalidArgument)
			}
			if _, _, err := SchemaVersions(t.Context(), db, history); !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Errorf("SchemaVersions = %v, want %s", err, ErrorCodeInvalidArgument)
			}
		})
	}
}

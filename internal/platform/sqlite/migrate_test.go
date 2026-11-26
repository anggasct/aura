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
		{{Version: 0}},
		{{Version: -1}},
		{{Version: 1}, {Version: 1}},
		{{Version: 2}, {Version: 1}},
	} {
		err := validateMigrationOrder(ms)
		if err == nil {
			t.Errorf("validateMigrationOrder(%+v) accepted an invalid list", ms)
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

func TestBaselineValidation(t *testing.T) {
	valid := StaticSchema{Name: "turn", SQL: `CREATE TABLE session (id TEXT PRIMARY KEY);`}
	migrations, err := Baseline(valid, StaticSchema{Name: "effect", SQL: `CREATE TABLE effect_intent (id TEXT PRIMARY KEY);`})
	if err != nil {
		t.Fatalf("Baseline valid: %v", err)
	}
	if len(migrations) != 2 || migrations[0].Version != 1 || migrations[0].Owner != "turn" || migrations[1].Version != 2 || migrations[1].Owner != "effect" {
		t.Fatalf("migrations = %+v", migrations)
	}

	cases := map[string][]SchemaOwner{
		"empty":          nil,
		"nil owner":      {valid, nil},
		"empty name":     {StaticSchema{Name: "", SQL: `CREATE TABLE a (id TEXT);`}},
		"whitespace SQL": {StaticSchema{Name: "a", SQL: " \t\n "}},
		"duplicate name": {valid, StaticSchema{Name: "turn", SQL: `CREATE TABLE other (id TEXT);`}},
	}
	for name, owners := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Baseline(owners...); !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("Baseline(%s) = %v, want %s", name, err, ErrorCodeInvalidArgument)
			}
		})
	}
}

func TestMigrateVerifiesOwnerAndSupportsSyntheticUpgrade(t *testing.T) {
	ctx := t.Context()
	db := openFresh(t, Options{})
	v1 := []Migration{
		{Version: 1, Owner: "turn", SQL: `CREATE TABLE session (id TEXT PRIMARY KEY);`},
	}
	if err := Migrate(ctx, db, v1); err != nil {
		t.Fatalf("Migrate v1: %v", err)
	}
	if err := Migrate(ctx, db, []Migration{{Version: 1, Owner: "", SQL: v1[0].SQL}}); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("Migrate empty owner = %v, want %s", err, ErrorCodeInvalidArgument)
	}
	if err := Migrate(ctx, db, []Migration{{Version: 1, Owner: "other", SQL: v1[0].SQL}}); !fault.HasCode(err, ErrorCodeMigrationChecksumMismatch) {
		t.Fatalf("Migrate owner mismatch = %v, want %s", err, ErrorCodeMigrationChecksumMismatch)
	}
	if err := Migrate(ctx, db, []Migration{{Version: 1, Owner: "turn", SQL: `CREATE TABLE session (id TEXT PRIMARY KEY, extra TEXT);`}}); !fault.HasCode(err, ErrorCodeMigrationChecksumMismatch) {
		t.Fatalf("Migrate checksum mismatch = %v, want %s", err, ErrorCodeMigrationChecksumMismatch)
	}

	v2 := []Migration{v1[0], {Version: 2, Owner: "turn", SQL: `ALTER TABLE session ADD COLUMN label TEXT NOT NULL DEFAULT '';`}}
	if err := Migrate(ctx, db, v2); err != nil {
		t.Fatalf("Migrate v2: %v", err)
	}
	applied, latest, err := SchemaVersions(ctx, db, v2)
	if err != nil || applied != 2 || latest != 2 {
		t.Fatalf("SchemaVersions = %d/%d, %v; want 2/2", applied, latest, err)
	}
}

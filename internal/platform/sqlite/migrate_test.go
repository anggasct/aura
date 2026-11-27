package sqlite

import (
	"slices"
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

func TestHistoryAppendsOwnerStepsAfterTheBaselines(t *testing.T) {
	turn := StaticSchema{Name: "turn", SQL: `CREATE TABLE session (id TEXT PRIMARY KEY);`}
	effect := StaticSchema{
		Name:  "effect",
		SQL:   `CREATE TABLE effect_intent (id TEXT PRIMARY KEY);`,
		Steps: []Step{{Version: 5, SQL: `ALTER TABLE effect_intent ADD COLUMN a TEXT;`}, {Version: 7, SQL: `ALTER TABLE effect_intent ADD COLUMN c TEXT;`}},
	}
	workflow := StaticSchema{
		Name:  "workflow",
		SQL:   `CREATE TABLE workflow_run (id TEXT PRIMARY KEY);`,
		Steps: []Step{{Version: 6, SQL: `ALTER TABLE workflow_run ADD COLUMN b TEXT;`}},
	}
	got, err := History(turn, effect, workflow)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	want := []Migration{
		{Version: 1, Owner: "turn", SQL: turn.SQL},
		{Version: 2, Owner: "effect", SQL: effect.SQL},
		{Version: 3, Owner: "workflow", SQL: workflow.SQL},
		{Version: 5, Owner: "effect", SQL: effect.Steps[0].SQL},
		{Version: 6, Owner: "workflow", SQL: workflow.Steps[0].SQL},
		{Version: 7, Owner: "effect", SQL: effect.Steps[1].SQL},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("History = %+v, want %+v", got, want)
	}
	if err := validateMigrationOrder(got); err != nil {
		t.Fatalf("History produced an invalid order: %v", err)
	}
}

func TestHistoryNumbersOnlyBaselineOwners(t *testing.T) {
	first := StaticSchema{Name: "first", SQL: `CREATE TABLE first_tbl (id TEXT PRIMARY KEY);`}
	stepsOnly := StaticSchema{Name: "stepsonly", Steps: []Step{{Version: 4, SQL: `ALTER TABLE first_tbl ADD COLUMN x TEXT;`}}}
	second := StaticSchema{Name: "second", SQL: `CREATE TABLE second_tbl (id TEXT PRIMARY KEY);`}
	got, err := History(first, stepsOnly, second)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	want := []Migration{
		{Version: 1, Owner: "first", SQL: first.SQL},
		{Version: 2, Owner: "second", SQL: second.SQL},
		{Version: 4, Owner: "stepsonly", SQL: stepsOnly.Steps[0].SQL},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("History = %+v, want %+v", got, want)
	}
}

func TestHistoryRefusesStepVersionsThatCollide(t *testing.T) {
	valid := StaticSchema{Name: "turn", SQL: `CREATE TABLE session (id TEXT PRIMARY KEY);`}
	step := func(version int) []Step {
		return []Step{{Version: version, SQL: `ALTER TABLE session ADD COLUMN x TEXT;`}}
	}

	cases := map[string][]SchemaOwner{
		"empty":                      nil,
		"nil owner":                  {valid, nil},
		"empty name":                 {StaticSchema{Name: "", SQL: `CREATE TABLE a (id TEXT);`}},
		"whitespace SQL":             {StaticSchema{Name: "a", SQL: " \t\n "}},
		"duplicate name":             {valid, StaticSchema{Name: "turn", SQL: `CREATE TABLE other (id TEXT);`}},
		"neither baseline nor steps": {valid, StaticSchema{Name: "empty"}},
		"duplicate version across owners": {
			StaticSchema{Name: "a", SQL: `CREATE TABLE a (id TEXT);`, Steps: step(5)},
			StaticSchema{Name: "b", SQL: `CREATE TABLE b (id TEXT);`, Steps: step(5)},
		},
		"version within the baselines": {
			StaticSchema{Name: "a", SQL: `CREATE TABLE a (id TEXT);`},
			StaticSchema{Name: "b", SQL: `CREATE TABLE b (id TEXT);`, Steps: step(2)},
		},
		"owner steps out of order": {
			StaticSchema{Name: "a", SQL: `CREATE TABLE a (id TEXT);`, Steps: []Step{{Version: 6, SQL: `SELECT 1;`}, {Version: 5, SQL: `SELECT 1;`}}},
		},
		"owner repeats its own version": {
			StaticSchema{Name: "a", SQL: `CREATE TABLE a (id TEXT);`, Steps: []Step{{Version: 5, SQL: `SELECT 1;`}, {Version: 5, SQL: `SELECT 1;`}}},
		},
		"empty step SQL": {
			StaticSchema{Name: "a", SQL: `CREATE TABLE a (id TEXT);`, Steps: []Step{{Version: 5, SQL: "  "}}},
		},
	}
	for name, owners := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := History(owners...); !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("History(%s) = %v, want %s", name, err, ErrorCodeInvalidArgument)
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

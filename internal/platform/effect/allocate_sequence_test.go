package effect

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

func toolSequences(t *testing.T, db *sql.DB) []uint64 {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT sequence FROM runtime_event WHERE kind = ? ORDER BY sequence ASC`, EventKindToolRequested)
	if err != nil {
		t.Fatalf("query sequences: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var sequences []uint64
	for rows.Next() {
		var sequence uint64
		if err := rows.Scan(&sequence); err != nil {
			t.Fatalf("scan sequence: %v", err)
		}
		sequences = append(sequences, sequence)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sequences: %v", err)
	}
	return sequences
}

func allocatingPrepare(n int) *PrepareRequest {
	req := validPrepare(n)
	req.EventSequence = 0
	req.AllocateSequence = true
	return req
}

func TestPrepareAllocatesTheToolRequestedSequence(t *testing.T) {
	t.Parallel()
	j, db := newTestJournal(t)
	for n := 1; n <= 3; n++ {
		mustPrepare(t, j, validPrepare(n))
	}

	mustPrepare(t, j, allocatingPrepare(4))
	mustPrepare(t, j, allocatingPrepare(5))
	if got, want := toolSequences(t, db), []uint64{1, 2, 3, 4, 5}; !slices.Equal(got, want) {
		t.Fatalf("sequences = %v, want %v", got, want)
	}

	replayed := mustPrepare(t, j, allocatingPrepare(5))
	if replayed.State != StatePrepared {
		t.Fatalf("replayed state = %s, want %s", replayed.State, StatePrepared)
	}
	if got := toolSequences(t, db); len(got) != 5 {
		t.Fatalf("a replay appended an event: sequences = %v", got)
	}

	t.Run("a sequence together with allocation is refused", func(t *testing.T) {
		req := allocatingPrepare(6)
		req.EventSequence = 9
		_, err := j.Prepare(t.Context(), req)
		assertCode(t, err, ErrorCodeInvalidArgument)
	})
	t.Run("no sequence and no allocation is refused", func(t *testing.T) {
		req := validPrepare(7)
		req.EventSequence = 0
		_, err := j.Prepare(t.Context(), req)
		assertCode(t, err, ErrorCodeInvalidArgument)
	})
	if got := toolSequences(t, db); len(got) != 5 {
		t.Fatalf("a refused prepare appended an event: sequences = %v", got)
	}
}

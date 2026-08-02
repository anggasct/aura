package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

// FuzzEventPayload drives the event-append validation with arbitrary payloads,
// sequences, and schema versions. The append must classify every invalid input
// as a typed error and never panic; the executor is stubbed so the fuzz
// exercises validation and boundary conversion, not SQLite.
func FuzzEventPayload(f *testing.F) {
	f.Add([]byte(`{"kind":"turn.completed"}`), uint64(1), uint16(1))
	f.Add([]byte(`not json`), uint64(1), uint16(1))
	f.Add([]byte(``), uint64(0), uint16(0))
	f.Add([]byte(`null`), uint64(1)<<62, uint16(1))
	f.Fuzz(func(t *testing.T, payload []byte, sequence uint64, schemaVersion uint16) {
		e := &RuntimeEvent{
			ID:            "fuzz",
			SessionID:     "session",
			TurnID:        "turn",
			InvocationID:  "invocation",
			Author:        "author",
			Kind:          "kind",
			Payload:       payload,
			Sequence:      sequence,
			SchemaVersion: schemaVersion,
			CreatedAt:     time.Now().UTC(),
		}
		exec := func(context.Context, ...any) (sql.Result, error) { return driver.RowsAffected(1), nil }
		err := appendEventCore(context.Background(), exec, e)
		if err != nil {
			var storeErr *Error
			if !errors.As(err, &storeErr) {
				t.Errorf("appendEventCore returned a non-typed error: %v", err)
			}
		}
	})
}

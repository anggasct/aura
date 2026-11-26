package turnstore_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func TestOwnerActionRecorderNilGuardAndUTC(t *testing.T) {
	var nilRec *turnstore.OwnerActionRecorder
	if err := nilRec.RecordOwnerRekey(t.Context(), principal.RekeyReport{}, time.Now()); !fault.HasCode(err, turnstore.ErrorCodeInvalidArgument) {
		t.Fatalf("nil RecordOwnerRekey = %v, want %s", err, turnstore.ErrorCodeInvalidArgument)
	}

	db := sqlitetest.OpenMigrated(t, turnstore.Schema())
	rec := turnstore.NewOwnerActionRecorder(db)
	zone := time.FixedZone("UTC+7", 7*3600)
	at := time.Date(2026, 11, 26, 14, 30, 0, 0, zone)
	if err := rec.RecordOwnerRekey(t.Context(), principal.RekeyReport{}, at); err != nil {
		t.Fatalf("RecordOwnerRekey: %v", err)
	}

	var eventID, payloadJSON, createdAt string
	if err := db.QueryRowContext(t.Context(), `SELECT id, payload_json, created_at FROM runtime_event WHERE kind = 'principal.rekey.v1'`).Scan(&eventID, &payloadJSON, &createdAt); err != nil {
		t.Fatalf("query rekey event: %v", err)
	}
	wantUTC := at.UTC().Format(time.RFC3339Nano)
	if !strings.HasSuffix(eventID, wantUTC) {
		t.Errorf("event id = %q, want UTC suffix %q", eventID, wantUTC)
	}
	var decoded struct {
		At string `json:"at"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &decoded); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if decoded.At != wantUTC {
		t.Errorf("payload.at = %q, want %q", decoded.At, wantUTC)
	}
	if !strings.HasSuffix(createdAt, "Z") {
		t.Errorf("created_at = %q, want UTC timestamp", createdAt)
	}
}

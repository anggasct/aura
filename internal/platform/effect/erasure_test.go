package effect

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/store/storetest"
)

func TestSessionApprovalEraserRefusesANilReader(t *testing.T) {
	db := storetest.OpenMigrated(t)
	if _, err := NewSessionApprovalEraser(db, nil, false); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("NewSessionApprovalEraser(db, nil, false) = %v, want %s", err, ErrorCodeInvalidArgument)
	}
}

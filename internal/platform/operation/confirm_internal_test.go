package operation

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestAnAgentCallIsNeverConfirmedWithoutACode(t *testing.T) {
	registry := &Registry{}
	coded := &Descriptor{Preview: true, Confirmation: ConfirmPreviewWithCode}
	plain := &Descriptor{Preview: true, Confirmation: ConfirmPreviewUntilApply}
	agent := &Call{Caller: Caller{Surface: SurfaceAgent}, PreviewDigest: "digest", Confirmation: "ABCD-EFGH"}

	if err := registry.confirmed(coded, agent); !fault.HasCode(err, ErrorCodeConfirmation) {
		t.Errorf("an agent call of a code-confirmed operation = %v, want %s", err, ErrorCodeConfirmation)
	}
	if err := registry.confirmed(plain, agent); err != nil {
		t.Errorf("an agent call of an operation that needs no code = %v, want it left to the other checks", err)
	}
}

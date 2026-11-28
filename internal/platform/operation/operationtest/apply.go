package operationtest

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/operation"
)

func (r *Rig) InvokePreviewed(tb testing.TB, id string, call *operation.Call) (operation.Result, error) {
	tb.Helper()
	descriptor, found := r.Registry.Descriptor(id)
	if !found {
		tb.Fatalf("operation %s is not registered", id)
	}
	bound := *call
	if descriptor.Preview {
		preview, err := r.Registry.Preview(tb.Context(), id, call)
		if err != nil {
			return operation.Result{}, err
		}
		bound.PreviewDigest = preview.Digest
	}
	return r.Registry.Invoke(tb.Context(), id, &bound)
}

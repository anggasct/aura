package operationtest

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/operation"
)

func InvokePreviewed(tb testing.TB, registry *operation.Registry, id string, call *operation.Call) (operation.Result, error) {
	tb.Helper()
	descriptor, found := registry.Descriptor(id)
	if !found {
		tb.Fatalf("operation %s is not registered", id)
	}
	bound := *call
	if descriptor.Preview {
		preview, err := registry.Preview(tb.Context(), id, call)
		if err != nil {
			return operation.Result{}, err
		}
		bound.PreviewDigest = preview.Digest
	}
	return registry.Invoke(tb.Context(), id, &bound)
}

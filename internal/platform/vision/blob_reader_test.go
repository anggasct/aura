package vision

import (
	"bytes"
	"context"
	"io"
	"testing"
)

func TestBlobReaderReadsWithinBound(t *testing.T) {
	reader := NewBlobReader(func(_ context.Context, refID string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte("hello-" + refID))), nil
	})
	got, err := reader.ReadBlob(t.Context(), "ref1", 16)
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if string(got) != "hello-ref1" {
		t.Fatalf("ReadBlob = %q, want hello-ref1", string(got))
	}
	if _, err := reader.ReadBlob(t.Context(), "ref1", 4); err == nil {
		t.Fatal("ReadBlob exceeded bound without error")
	}
}

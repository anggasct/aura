package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"testing"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
	"github.com/anggasct/aura/internal/telemetry"
	"github.com/anggasct/aura/internal/vision"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type fakeArtifactStore struct {
	blobs map[string][]byte
}

func (f *fakeArtifactStore) Put(ctx context.Context, r io.Reader, meta *store.ArtifactMetadata) (store.ArtifactRef, error) {
	return store.ArtifactRef{}, nil
}

func (f *fakeArtifactStore) Open(ctx context.Context, refID string) (io.ReadCloser, store.ArtifactMetadata, error) {
	data, ok := f.blobs[refID]
	if !ok {
		return nil, store.ArtifactMetadata{}, sql.ErrNoRows
	}
	return io.NopCloser(bytes.NewReader(data)), store.ArtifactMetadata{ID: refID}, nil
}

func (f *fakeArtifactStore) Link(ctx context.Context, link *store.ArtifactLink) error {
	return nil
}

func (f *fakeArtifactStore) Unlink(ctx context.Context, refID string) error {
	return nil
}

func TestVisionBlobReaderRoundTrip(t *testing.T) {
	reader := &visionBlobReader{blobs: &fakeArtifactStore{blobs: map[string][]byte{"art-1": []byte("bytes")}}}
	data, err := reader.ReadBlob(context.Background(), "art-1", 1024)
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if string(data) != "bytes" {
		t.Fatalf("data = %q", data)
	}
}

func TestVisionBlobReaderFailsClosed(t *testing.T) {
	reader := &visionBlobReader{blobs: &fakeArtifactStore{blobs: map[string][]byte{"art-1": []byte("bytes")}}}
	for name, fn := range map[string]func() error{
		"missing":       func() error { _, err := reader.ReadBlob(context.Background(), "nope", 1024); return err },
		"over bound":    func() error { _, err := reader.ReadBlob(context.Background(), "art-1", 2); return err },
		"empty ref":     func() error { _, err := reader.ReadBlob(context.Background(), "  ", 1024); return err },
		"invalid bound": func() error { _, err := reader.ReadBlob(context.Background(), "art-1", 0); return err },
		"nil context": func() error {
			var missing context.Context
			_, err := reader.ReadBlob(missing, "art-1", 1024)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(); err == nil {
				t.Fatal("expected failure")
			}
		})
	}
	var nilReader *visionBlobReader
	if _, err := nilReader.ReadBlob(context.Background(), "art-1", 1024); err == nil {
		t.Fatal("expected nil store rejection")
	}
}

func TestVisionBlobReaderPreservesStoreCause(t *testing.T) {
	sentinel := sql.ErrNoRows
	reader := &visionBlobReader{blobs: &fakeArtifactStore{blobs: map[string][]byte{}}}
	_, err := reader.ReadBlob(context.Background(), "missing", 1024)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("store cause is not reachable: %v", err)
	}
}

func TestVisionWiringForConfig(t *testing.T) {
	empty := &config.Config{Vision: nil}
	if wiring := visionWiringForConfig(empty, &sql.DB{}, t.TempDir()); wiring.Policy != nil || wiring.Blobs != nil {
		t.Fatal("nil vision section must yield empty wiring")
	}
	if wiring := visionWiringForConfig(nil, &sql.DB{}, t.TempDir()); wiring.Policy != nil || wiring.Blobs != nil {
		t.Fatal("nil config must yield empty wiring")
	}
	enabled := &config.Config{Vision: &config.Vision{Enabled: true}}
	if wiring := visionWiringForConfig(enabled, nil, t.TempDir()); wiring.Policy != nil || wiring.Blobs != nil {
		t.Fatal("nil database must yield empty wiring")
	}
	if wiring := visionWiringForConfig(enabled, &sql.DB{}, ""); wiring.Policy != nil || wiring.Blobs != nil {
		t.Fatal("empty artifact root must yield empty wiring")
	}
	wiring := visionWiringForConfig(enabled, &sql.DB{}, t.TempDir())
	if wiring.Policy == nil || wiring.Blobs == nil {
		t.Fatal("enabled vision must yield policy and blob reader")
	}
}

func TestNewVisionServiceDisabledYieldsNil(t *testing.T) {
	if svc := newVisionService(nil, &sql.DB{}, t.TempDir()); svc != nil {
		t.Fatal("nil config must yield nil service")
	}
	disabled := &config.Config{Vision: &config.Vision{Enabled: false}}
	if svc := newVisionService(disabled, &sql.DB{}, t.TempDir()); svc != nil {
		t.Fatal("disabled vision must yield nil service")
	}
	enabled := &config.Config{Vision: &config.Vision{Enabled: true}}
	if svc := newVisionService(enabled, nil, t.TempDir()); svc != nil {
		t.Fatal("nil database must yield nil service")
	}
	if svc := newVisionService(enabled, &sql.DB{}, ""); svc != nil {
		t.Fatal("empty root must yield nil service")
	}
}

func TestVisionStoreAdapterFailsClosed(t *testing.T) {
	var nilAdapter *visionStoreAdapter
	if _, err := nilAdapter.Put(t.Context(), bytes.NewReader([]byte("x")), nil); err == nil {
		t.Fatal("nil adapter Put must fail")
	}
	if err := nilAdapter.Unlink(t.Context(), "art-1"); err == nil {
		t.Fatal("nil adapter Unlink must fail")
	}
	adapter := &visionStoreAdapter{}
	var nilCtx context.Context
	if _, err := adapter.Put(nilCtx, bytes.NewReader([]byte("x")), nil); err == nil {
		t.Fatal("nil context must fail")
	}
}

func TestVisionRecorderObserverForwardsBuckets(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	recorder, err := telemetry.NewVisionRecorder(mp)
	if err != nil {
		t.Fatalf("NewVisionRecorder: %v", err)
	}
	observer := visionRecorderObserver(recorder)
	if observer == nil {
		t.Fatal("observer is nil")
	}
	observer(context.Background(), &vision.Observation{
		Operation:    "ingest",
		Result:       "ok",
		MIME:         "image/png",
		Images:       1,
		Version:      "v1",
		EncodedBytes: 1 << 16,
		Pixels:       1 << 18,
	})
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != telemetry.MetricVisionOperationsTotal {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key(telemetry.AttrVisionImages)); ok && v.AsInt64() == 1 {
					if _, ok := dp.Attributes.Value(attribute.Key(telemetry.AttrVisionSizeBucket)); ok {
						if _, ok := dp.Attributes.Value(attribute.Key(telemetry.AttrVisionPixelsBucket)); ok {
							found = true
						}
					}
				}
			}
		}
	}
	if !found {
		t.Error("observer did not forward images/size/pixels bucket labels")
	}
}

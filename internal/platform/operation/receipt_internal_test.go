package operation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
)

func sampleReceipt(key string, at time.Time) *receipt {
	return &receipt{key: key, operation: "stub.change", digest: "digest", surface: SurfaceAgent, result: json.RawMessage(`{"done":true}`), createdAt: at}
}

func TestReceiptOmitsSecretOutput(t *testing.T) {
	descriptor := &Descriptor{
		ID: "stub.host-secret",
		Output: Output{Kind: OutputRecord, Fields: []OutputField{
			{Name: "id"}, {Name: "token", Secret: true}, {Name: "recovery", Secret: true},
		}},
	}
	cases := []struct {
		name    string
		data    string
		want    string
		wantErr bool
	}{
		{name: "secret fields are removed", data: `{"id":"a","token":"s3cret","recovery":"r"}`, want: `{"id":"a"}`},
		{name: "a result without them is unchanged", data: `{"id":"a"}`, want: `{"id":"a"}`},
		{name: "a result that is not an object fails", data: `[1]`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := receiptData(descriptor, json.RawMessage(tc.data))
			if tc.wantErr {
				if !fault.HasCode(err, ErrorCodeFailed) {
					t.Fatalf("receiptData = %v, want operation_failed", err)
				}
				return
			}
			if err != nil || string(got) != tc.want {
				t.Fatalf("receiptData = %s, %v; want %s", got, err, tc.want)
			}
		})
	}
	t.Run("a descriptor without secrets passes the data through", func(t *testing.T) {
		plain := &Descriptor{ID: "stub.plain", Output: Output{Kind: OutputRecord, Fields: []OutputField{{Name: "id"}}}}
		got, err := receiptData(plain, json.RawMessage(`[1]`))
		if err != nil || string(got) != `[1]` {
			t.Fatalf("receiptData = %s, %v; want the data unchanged", got, err)
		}
	})
}

func TestReceiptStoreRefusesTheSecondWriterOfAKey(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schema())
	store := &receiptStore{db: db}
	at := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)

	if err := store.insert(t.Context(), sampleReceipt("key-1", at)); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := store.insert(t.Context(), sampleReceipt("key-1", at)); !errors.Is(err, errReceiptExists) {
		t.Fatalf("second insert = %v, want the receipt exists error", err)
	}
	found, ok, err := store.lookup(t.Context(), "key-1")
	if err != nil || !ok || found.digest != "digest" || !found.createdAt.Equal(at) || found.surface != SurfaceAgent {
		t.Fatalf("lookup = %+v, %v, %v; want the first receipt", found, ok, err)
	}
	if _, ok, err := store.lookup(t.Context(), "missing"); ok || err != nil {
		t.Fatalf("lookup of a missing key = %v, %v; want absent", ok, err)
	}
}

func TestReceiptStoreSerializesConcurrentWriters(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schema())
	store := &receiptStore{db: db}
	const writers = 8
	var wg sync.WaitGroup
	results := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = sqlite.RunInTx(t.Context(), db, func(ctx context.Context) error {
				return store.insert(ctx, sampleReceipt("key-1", time.Now()))
			})
		}()
	}
	wg.Wait()
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, errReceiptExists) {
			t.Errorf("a writer failed with %v, want success or the receipt exists error", err)
		}
	}
	if winners != 1 {
		t.Errorf("writers that inserted = %d, want 1", winners)
	}
}

func TestReceiptStoreRejectsMalformedReceipts(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schema())
	store := &receiptStore{db: db}
	at := time.Now()
	cases := []struct {
		name string
		row  *receipt
	}{
		{name: "a nil receipt", row: nil},
		{name: "an empty key", row: sampleReceipt("", at)},
		{name: "a key over 256 bytes", row: sampleReceipt(strings.Repeat("k", 257), at)},
		{name: "a CLI receipt", row: &receipt{key: "k", surface: SurfaceCLI, result: json.RawMessage(`{}`), createdAt: at}},
		{name: "a result that is not JSON", row: &receipt{key: "k", surface: SurfaceAgent, result: json.RawMessage(`{`), createdAt: at}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := store.insert(t.Context(), tc.row); !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Fatalf("insert = %v, want invalid_argument", err)
			}
		})
	}
}

func TestPruneReceiptsDeletesOnlyOldRows(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schema())
	store := &receiptStore{db: db}
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	for key, age := range map[string]time.Duration{"old-1": 31 * 24 * time.Hour, "old-2": 45 * 24 * time.Hour, "fresh": 29 * 24 * time.Hour} {
		if err := store.insert(t.Context(), sampleReceipt(key, now.Add(-age))); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}

	deleted, err := PruneReceipts(t.Context(), db, now.Add(-ReceiptRetention))

	if err != nil || deleted != 2 {
		t.Fatalf("PruneReceipts = %d, %v; want 2 deleted", deleted, err)
	}
	if _, ok, _ := store.lookup(t.Context(), "fresh"); !ok {
		t.Error("the fresh receipt was deleted")
	}
	if _, err := PruneReceipts(t.Context(), nil, now); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("PruneReceipts without a database = %v, want invalid_argument", err)
	}
	if _, err := PruneReceipts(t.Context(), db, time.Time{}); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("PruneReceipts without a cutoff = %v, want invalid_argument", err)
	}
}

func TestWinnerReturnsTheReceiptThatTookTheKey(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schema())
	registry := &Registry{receipts: receiptStore{db: db}}
	if _, err := registry.winner(t.Context(), &Call{IdempotencyKey: "key-1", RequestDigest: "digest"}); !fault.HasCode(err, ErrorCodeFailed) {
		t.Fatalf("winner without a receipt = %v, want operation_failed", err)
	}
	if err := registry.receipts.insert(t.Context(), sampleReceipt("key-1", time.Now())); err != nil {
		t.Fatalf("insert: %v", err)
	}

	result, err := registry.winner(t.Context(), &Call{IdempotencyKey: "key-1", RequestDigest: "digest"})
	if err != nil || string(result.Data) != `{"done":true}` {
		t.Fatalf("winner = %s, %v; want the recorded result", result.Data, err)
	}
	if _, err := registry.winner(t.Context(), &Call{IdempotencyKey: "key-1", RequestDigest: "other"}); !fault.HasCode(err, ErrorCodeKeyReused) {
		t.Fatalf("winner with another digest = %v, want operation_key_reused", err)
	}
}

func TestPruneComparesReceiptTimesAcrossFractions(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schema())
	store := &receiptStore{db: db}
	base := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	rows := map[string]time.Time{
		"whole":    base,
		"tenth":    base.Add(100 * time.Millisecond),
		"half":     base.Add(500 * time.Millisecond),
		"nanos":    base.Add(500*time.Millisecond + 1),
		"next":     base.Add(time.Second),
		"previous": base.Add(-time.Nanosecond),
	}
	for key, at := range rows {
		if err := store.insert(t.Context(), sampleReceipt(key, at)); err != nil {
			t.Fatalf("insert %s: %v", key, err)
		}
	}

	deleted, err := PruneReceipts(t.Context(), db, base.Add(500*time.Millisecond))

	if err != nil || deleted != 3 {
		t.Fatalf("PruneReceipts = %d, %v; want the three receipts before the cutoff", deleted, err)
	}
	for _, key := range []string{"half", "nanos", "next"} {
		if _, ok, _ := store.lookup(t.Context(), key); !ok {
			t.Errorf("the receipt %s at or after the cutoff was deleted", key)
		}
	}
	found, ok, err := store.lookup(t.Context(), "half")
	if err != nil || !ok || !found.createdAt.Equal(base.Add(500*time.Millisecond)) {
		t.Fatalf("lookup = %+v, %v, %v; want the stored time back with its fraction", found, ok, err)
	}
}

package usage

import (
	"os"
	"testing"
	"time"
)

func testPrice(modelDef string) *Price {
	return &Price{
		ModelDefinitionID:       modelDef,
		Currency:                "USD",
		MicrosPerInputToken:     10,
		MicrosPerOutputToken:    30,
		MicrosPerCacheToken:     2,
		MicrosPerReasoningToken: 5,
		EffectiveFrom:           time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		MaxReservationRate:      200,
		Source:                  "test",
	}
}

func TestPriceReserveCostConservative(t *testing.T) {
	p := testPrice("primary")
	cases := []struct {
		name   string
		input  int64
		output int64
		want   int64
	}{
		{"zero usage reserves one micro", 0, 0, 1},
		{"known input and max output", 100, 200, int64(100*10+200*30) * 200 / 100},
		{"unknown output reserves input only", 100, 0, int64(100*10) * 200 / 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := p.ReserveCostMicros(tc.input, tc.output)
			if got != tc.want {
				t.Errorf("ReserveCostMicros(%d, %d) = %d, want %d", tc.input, tc.output, got, tc.want)
			}
			if got < 1 {
				t.Errorf("reservation must never be zero, got %d", got)
			}
		})
	}
}

func TestPriceCostDeterministic(t *testing.T) {
	p := testPrice("primary")
	u := Usage{InputTokens: 1000, OutputTokens: 500, CacheTokens: 200, ReasoningTokens: 50}
	want := int64(1000*10 + 500*30 + 200*2 + 50*5)
	got := p.CostMicros(u)
	if got != want {
		t.Errorf("CostMicros = %d, want %d", got, want)
	}
	if got != p.CostMicros(u) {
		t.Errorf("CostMicros is not deterministic")
	}
}

func TestPriceEffectiveBoundaries(t *testing.T) {
	reg := NewPriceRegistry()
	v1 := testPrice("primary")
	v1.EffectiveFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	v2 := testPrice("primary")
	v2.MicrosPerOutputToken = 40
	v2.EffectiveFrom = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := reg.Put(v1); err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(v2); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		at   time.Time
		want int64
	}{
		{"before v1", time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC), 0},
		{"v1 interval", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), 30},
		{"v2 interval", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := reg.At("primary", "USD", tc.at)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == 0 {
				if p != nil {
					t.Errorf("expected no price at %s, got %+v", tc.name, p)
				}
				return
			}
			if p == nil {
				t.Fatalf("expected price at %s", tc.name)
			}
			if p.MicrosPerOutputToken != tc.want {
				t.Errorf("output rate = %d, want %d", p.MicrosPerOutputToken, tc.want)
			}
		})
	}
}

func TestPriceRegistryRejectsInvalid(t *testing.T) {
	reg := NewPriceRegistry()
	bad := testPrice("primary")
	bad.MaxReservationRate = 50 // below 100
	if err := reg.Put(bad); err == nil {
		t.Error("expected rejection of reservation rate below 100")
	}
	bad2 := testPrice("primary")
	bad2.MicrosPerInputToken = -1
	if err := reg.Put(bad2); err == nil {
		t.Error("expected rejection of negative rate")
	}
}

func TestPriceRegistryThreadSafe(t *testing.T) {
	reg := NewPriceRegistry()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 {
			p := testPrice("primary")
			if err := reg.Put(p); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for range 1000 {
		_, _ = reg.At("primary", "USD", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	}
	<-done
}

func TestPricesFileLoader(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/prices.yaml"
	writeFile(t, path, `
version: 1
prices:
  - model_definition_id: primary
    currency: USD
    micros_per_input_token: 10
    micros_per_output_token: 30
    effective_from: "2026-01-01T00:00:00Z"
    source: test
    max_reservation_rate: 200
`)
	reg := NewPriceRegistry()
	if err := LoadPricesFile(path, reg); err != nil {
		t.Fatalf("LoadPricesFile: %v", err)
	}
	p, err := reg.At("primary", "USD", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("expected price after load")
	}
	if p.MicrosPerInputToken != 10 {
		t.Errorf("input rate = %d, want 10", p.MicrosPerInputToken)
	}
}

func TestPricesFileLoaderRejectsBadVersion(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/prices.yaml"
	writeFile(t, path, "version: 99\nprices: []\n")
	err := LoadPricesFile(path, NewPriceRegistry())
	if err == nil {
		t.Fatal("expected error for unsupported version")
	}
	if code, ok := CodeOf(err); !ok || code != ErrorCodePriceVersionInvalid {
		t.Errorf("code = %v, want %v", code, ErrorCodePriceVersionInvalid)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

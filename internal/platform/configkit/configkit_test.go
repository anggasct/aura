package configkit

import "testing"

func TestByteSizeParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want ByteSize
	}{
		{raw: "1000", want: 1000},
		{raw: "5GiB", want: 5 << 30},
		{raw: "512MiB", want: 512 << 20},
		{raw: "2KiB", want: 2 << 10},
		{raw: "1TiB", want: 1 << 40},
		{raw: "2GB", want: 2000 * 1000 * 1000},
		{raw: "1500KB", want: 1500 * 1000},
	}
	for _, tc := range cases {
		var got ByteSize
		if err := got.UnmarshalText([]byte(tc.raw)); err != nil {
			t.Errorf("UnmarshalText(%q): %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("UnmarshalText(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
	for _, bad := range []string{"", "-5", "5XB", "abc", "5GiBx", "99999999999999999999TiB"} {
		var got ByteSize
		if err := got.UnmarshalText([]byte(bad)); err == nil {
			t.Errorf("UnmarshalText(%q) accepted an invalid size", bad)
		}
	}
	if got := (ByteSize(5 << 30)).String(); got != "5GiB" {
		t.Errorf("String() = %q, want 5GiB", got)
	}
}

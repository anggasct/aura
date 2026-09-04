package mcp

import "testing"

func TestProtocolVersions(t *testing.T) {
	testCases := []struct {
		version string
		valid   bool
	}{
		{ProtocolVersion20241105, true},
		{ProtocolVersion20250326, true},
		{ProtocolVersion20250618, true},
		{ProtocolVersion20251125, true},
		{ProtocolVersion20260728, true},
		{"2023-01-01", false},
		{"1.0.0", false},
		{"", false},
		{"unknown", false},
	}

	for _, tc := range testCases {
		t.Run(tc.version, func(t *testing.T) {
			got := IsSupportedProtocolVersion(tc.version)
			if got != tc.valid {
				t.Fatalf("IsSupportedProtocolVersion(%q) = %v, want %v", tc.version, got, tc.valid)
			}
		})
	}
}

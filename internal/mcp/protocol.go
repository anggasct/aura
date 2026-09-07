package mcp

import "slices"

const (
	ProtocolVersion20241105 = "2024-11-05"
	ProtocolVersion20250326 = "2025-03-26"
	ProtocolVersion20250618 = "2025-06-18"
	ProtocolVersion20251125 = "2025-11-25"
	ProtocolVersion20260728 = "2026-07-28"
)

var SupportedProtocolVersions = []string{
	ProtocolVersion20241105,
	ProtocolVersion20250326,
	ProtocolVersion20250618,
	ProtocolVersion20251125,
	ProtocolVersion20260728,
}

func IsSupportedProtocolVersion(version string) bool {
	return slices.Contains(SupportedProtocolVersions, version)
}

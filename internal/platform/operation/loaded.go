package operation

import (
	"slices"

	"github.com/anggasct/aura/internal/platform/tools"
)

func RecordedLoads(response map[string]any) []string {
	raw, ok := response["loaded"].([]any)
	if !ok || len(raw) > MaxLoadedPerSearch {
		return nil
	}
	names := make([]string, 0, len(raw))
	for _, entry := range raw {
		name, ok := entry.(string)
		if !ok || !tools.IsOperationEntry(name) || name == tools.OperationSearchToolName || len(name) > maxToolNameBytes {
			return nil
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

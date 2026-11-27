package turn

import (
	"strings"
)

func IsToolError(response map[string]any) bool {
	for key, value := range response {
		if !strings.EqualFold(key, "error") {
			continue
		}
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return true
			}
		case nil:
		default:
			return true
		}
	}
	return false
}

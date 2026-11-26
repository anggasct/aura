package knowledge

import (
	"strings"

	"github.com/anggasct/aura/internal/platform/secret"
)

func ResolveEmbeddingCredential(ref secret.Ref) (string, bool) {
	if ref.IsZero() {
		return "", true
	}
	value, err := ref.Reference().Resolve()
	value = strings.TrimSpace(value)
	if err != nil || value == "" {
		return "", false
	}
	return value, true
}

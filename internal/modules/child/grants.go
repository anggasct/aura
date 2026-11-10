package child

import (
	"slices"
	"strings"
)

func attenuateGrants(parent, requested []Grant) ([]Grant, error) {
	allowed := make(map[string]bool, len(parent))
	for _, grant := range parent {
		name := strings.TrimSpace(grant.Capability)
		if name == "" {
			continue
		}
		allowed[strings.ToLower(name)] = true
	}
	out := make([]Grant, 0, len(requested))
	for _, grant := range requested {
		name := strings.TrimSpace(grant.Capability)
		if name == "" {
			return nil, Errorf(ErrorCodeInvalidArgument, "child grant must carry a capability")
		}
		if strings.EqualFold(name, "spawn_child") {
			return nil, Errorf(ErrorCodeChildSpawnDenied, "child catalogs cannot carry spawn_child")
		}
		if len(name) > maxGrantChars {
			return nil, Errorf(ErrorCodeInvalidArgument, "child grant length is out of range")
		}
		if !allowed[strings.ToLower(name)] {
			return nil, Errorf(ErrorCodeChildInvalid, "child grant is outside the parent grants")
		}
		out = append(out, Grant{Capability: name, Scope: grant.Scope})
	}
	slices.SortFunc(out, func(a, b Grant) int {
		if a.Capability != b.Capability {
			return strings.Compare(a.Capability, b.Capability)
		}
		return strings.Compare(a.Scope, b.Scope)
	})
	return out, nil
}

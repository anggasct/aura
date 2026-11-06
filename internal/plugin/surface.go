package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/tools"
)

const (
	SurfaceTool       = "tool"
	SurfaceCapability = "capability"
	SurfaceEgress     = "egress"
	SurfaceSecret     = "secret"
	SurfaceBackend    = "backend"
	SurfaceAuth       = "auth"
	SurfaceBundle     = "bundle"

	ChangeAdded   = "added"
	ChangeChanged = "changed"
	ChangeRemoved = "removed"

	DetailTierRaised         = "tier raised"
	DetailTierLowered        = "tier lowered"
	DetailDescriptionChanged = "description changed"
	DetailDisplayChanged     = "approval display changed"
	DetailRefChanged         = "reference changed"
)

type GrantKey struct {
	Surface string
	Value   string
}

type Grant struct {
	GrantKey
	Fingerprint string
}

type ToolSurface struct {
	Tier        string
	Description string
	Display     []string
	Fingerprint string
}

type Surface struct {
	Backend string
	Auth    string
	Bundle  string
	Tools   map[string]ToolSurface
	Values  map[GrantKey]string
}

type Change struct {
	Surface string
	Value   string
	Kind    string
	Detail  string
}

type Diff struct {
	Restart []Change
	Widened []Change
	Removed []Change
}

func (d *Diff) Quarantines() bool { return len(d.Restart) > 0 }

func (d *Diff) Widens() bool { return len(d.Widened) > 0 }

func (d *Diff) Empty() bool {
	return len(d.Restart) == 0 && len(d.Widened) == 0 && len(d.Removed) == 0
}

func fingerprint(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func ToolFingerprint(tool *Tool) string {
	parts := []string{SurfaceTool, tool.Name, tool.Tier, tool.Description}
	if len(tool.Display) > 0 {
		parts = append(parts, "display", strings.Join(tool.Display, ","))
	}
	return fingerprint(parts...)
}

func ValueFingerprint(surface, value string) string {
	return fingerprint(surface, value)
}

func SecretFingerprint(secret *Secret) string {
	return fingerprint(SurfaceSecret, secret.Name, secret.Ref)
}

func jsonFingerprint(kind string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", Wrap(ErrManifestInvalid, err, "the manifest cannot be encoded")
	}
	return fingerprint(kind, string(data)), nil
}

func SurfaceOf(manifest *Manifest, bundleSHA256 string) (Surface, error) {
	normalized := manifest.normalized()
	backend, err := jsonFingerprint(SurfaceBackend, normalized.Backend)
	if err != nil {
		return Surface{}, err
	}
	auth, err := jsonFingerprint(SurfaceAuth, normalized.Auth)
	if err != nil {
		return Surface{}, err
	}
	surface := Surface{
		Backend: backend,
		Auth:    auth,
		Bundle:  bundleSHA256,
		Tools:   make(map[string]ToolSurface, len(normalized.Tools)),
		Values:  map[GrantKey]string{},
	}
	for i := range normalized.Tools {
		tool := &normalized.Tools[i]
		surface.Tools[tool.Name] = ToolSurface{Tier: tool.Tier, Description: tool.Description, Display: slices.Clone(tool.Display), Fingerprint: ToolFingerprint(tool)}
	}
	for _, capability := range normalized.Capabilities {
		surface.Values[GrantKey{SurfaceCapability, capability}] = ValueFingerprint(SurfaceCapability, capability)
	}
	for _, host := range normalized.Egress {
		surface.Values[GrantKey{SurfaceEgress, host}] = ValueFingerprint(SurfaceEgress, host)
	}
	for i := range normalized.Secrets {
		surface.Values[GrantKey{SurfaceSecret, normalized.Secrets[i].Name}] = SecretFingerprint(&normalized.Secrets[i])
	}
	return surface, nil
}

func (s *Surface) Grants() []Grant {
	grants := make([]Grant, 0, len(s.Tools)+len(s.Values))
	for name, tool := range s.Tools {
		grants = append(grants, Grant{GrantKey: GrantKey{SurfaceTool, name}, Fingerprint: tool.Fingerprint})
	}
	for key, fingerprint := range s.Values {
		grants = append(grants, Grant{GrantKey: key, Fingerprint: fingerprint})
	}
	slices.SortFunc(grants, compareGrants)
	return grants
}

func compareGrants(a, b Grant) int {
	if c := strings.Compare(a.Surface, b.Surface); c != 0 {
		return c
	}
	return strings.Compare(a.Value, b.Value)
}

func tierRank(tier string) int {
	if tier == TierEffect {
		return 1
	}
	return 0
}

func Compare(approved, candidate *Surface) Diff {
	var diff Diff
	if approved.Backend != candidate.Backend {
		diff.Restart = append(diff.Restart, Change{Surface: SurfaceBackend, Kind: ChangeChanged})
	}
	if approved.Auth != candidate.Auth {
		diff.Restart = append(diff.Restart, Change{Surface: SurfaceAuth, Kind: ChangeChanged})
	}
	if approved.Bundle != candidate.Bundle {
		diff.Restart = append(diff.Restart, Change{Surface: SurfaceBundle, Kind: ChangeChanged})
	}
	for _, name := range sortedKeys(candidate.Tools) {
		next := candidate.Tools[name]
		previous, known := approved.Tools[name]
		switch {
		case !known:
			diff.Widened = append(diff.Widened, Change{Surface: SurfaceTool, Value: name, Kind: ChangeAdded})
		case previous.Fingerprint != next.Fingerprint:
			diff.Widened = append(diff.Widened, Change{Surface: SurfaceTool, Value: name, Kind: ChangeChanged, Detail: toolChangeDetail(previous, next)})
		}
	}
	for _, name := range sortedKeys(approved.Tools) {
		if _, kept := candidate.Tools[name]; !kept {
			diff.Removed = append(diff.Removed, Change{Surface: SurfaceTool, Value: name, Kind: ChangeRemoved})
		}
	}
	for _, key := range sortedGrantKeys(candidate.Values) {
		previous, known := approved.Values[key]
		switch {
		case !known:
			diff.Widened = append(diff.Widened, Change{Surface: key.Surface, Value: key.Value, Kind: ChangeAdded})
		case previous != candidate.Values[key]:
			diff.Widened = append(diff.Widened, Change{Surface: key.Surface, Value: key.Value, Kind: ChangeChanged, Detail: DetailRefChanged})
		}
	}
	for _, key := range sortedGrantKeys(approved.Values) {
		if _, kept := candidate.Values[key]; !kept {
			diff.Removed = append(diff.Removed, Change{Surface: key.Surface, Value: key.Value, Kind: ChangeRemoved})
		}
	}
	return diff
}

func toolChangeDetail(previous, next ToolSurface) string {
	switch {
	case tierRank(next.Tier) > tierRank(previous.Tier):
		return DetailTierRaised
	case tierRank(next.Tier) < tierRank(previous.Tier):
		return DetailTierLowered
	case previous.Description != next.Description:
		return DetailDescriptionChanged
	default:
		return DetailDisplayChanged
	}
}

func sortedKeys[V any](values map[string]V) []string {
	return slices.Sorted(maps.Keys(values))
}

func sortedGrantKeys(values map[GrantKey]string) []GrantKey {
	return slices.SortedFunc(maps.Keys(values), func(a, b GrantKey) int {
		if c := strings.Compare(a.Surface, b.Surface); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})
}

func ToolID(plugin, tool string) string {
	return tools.PluginToolID(plugin, tool)
}

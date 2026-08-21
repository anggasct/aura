package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
)

type TrustLabel string

const (
	TrustOwnerInput        TrustLabel = "owner_input"
	TrustTrustedConfig     TrustLabel = "trusted_configuration"
	TrustUntrustedExternal TrustLabel = "untrusted_external"
	TrustDerivedUntrusted  TrustLabel = "derived_untrusted"
)

type RiskClass string

const (
	RiskReadOnly     RiskClass = "read_only"
	RiskEffectful    RiskClass = "effectful"
	RiskIrreversible RiskClass = "irreversible"
)

type Descriptor struct {
	Name           string
	Version        string
	SchemaDigest   string
	Capability     string
	Trust          TrustLabel
	Risk           RiskClass
	ScopeSummary   string
	MaxResultBytes int
}

type Catalog struct {
	descriptors map[string]Descriptor
	maxVisible  int
}

var descriptorNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

const maxDescriptorResultBytes = 64 << 20

func NewCatalog(descriptors []Descriptor, maxVisible int) (Catalog, error) {
	if maxVisible <= 0 {
		return Catalog{}, invalidArgument("catalog visible limit must be positive")
	}
	catalog := Catalog{descriptors: make(map[string]Descriptor, len(descriptors)), maxVisible: maxVisible}
	var problems []error
	for _, descriptor := range descriptors {
		if err := validateDescriptor(&descriptor); err != nil {
			problems = append(problems, err)
			continue
		}
		if _, exists := catalog.descriptors[descriptor.Name]; exists {
			problems = append(problems, codedError(ErrorCodeCatalogInvalid, "duplicate descriptor "+descriptor.Name, nil))
			continue
		}
		catalog.descriptors[descriptor.Name] = descriptor
	}
	if err := errors.Join(problems...); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func (c Catalog) Visible(capabilities []string, limit int) []Descriptor {
	if limit <= 0 || limit > c.maxVisible {
		limit = c.maxVisible
	}
	allowed := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		allowed[capability] = struct{}{}
	}
	names := make([]string, 0, len(c.descriptors))
	for name, descriptor := range c.descriptors {
		if _, ok := allowed[descriptor.Capability]; ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if len(names) > limit {
		names = names[:limit]
	}
	visible := make([]Descriptor, 0, len(names))
	for _, name := range names {
		visible = append(visible, c.descriptors[name])
	}
	return visible
}

func (c Catalog) Select(name string, capabilities []string) (Descriptor, error) {
	descriptor, ok := c.descriptors[name]
	if !ok {
		return Descriptor{}, codedError(ErrorCodeCapabilityUnavailable, "descriptor is not registered", nil)
	}
	if !slices.Contains(capabilities, descriptor.Capability) {
		return Descriptor{}, codedError(ErrorCodeCapabilityUnavailable, "descriptor capability is unavailable", nil)
	}
	return descriptor, nil
}

func validateDescriptor(descriptor *Descriptor) error {
	if descriptor == nil {
		return invalidArgument("descriptor must not be nil")
	}
	if !descriptorNamePattern.MatchString(descriptor.Name) || descriptor.Version == "" || descriptor.Capability == "" || descriptor.ScopeSummary == "" {
		return codedError(ErrorCodeCatalogInvalid, "descriptor identity or scope is invalid", nil)
	}
	if !validTrust(descriptor.Trust) || !validRisk(descriptor.Risk) {
		return codedError(ErrorCodeCatalogInvalid, "descriptor trust or risk is invalid", nil)
	}
	if descriptor.MaxResultBytes <= 0 || descriptor.MaxResultBytes > maxDescriptorResultBytes {
		return codedError(ErrorCodeCatalogInvalid, "descriptor result bound must be positive", nil)
	}
	if len(descriptor.SchemaDigest) != sha256.Size*2 {
		return codedError(ErrorCodeCatalogInvalid, "descriptor schema digest is invalid", nil)
	}
	if _, err := hex.DecodeString(descriptor.SchemaDigest); err != nil {
		return codedError(ErrorCodeCatalogInvalid, "descriptor schema digest is invalid", err)
	}
	return nil
}

func validTrust(trust TrustLabel) bool {
	switch trust {
	case TrustOwnerInput, TrustTrustedConfig, TrustUntrustedExternal, TrustDerivedUntrusted:
		return true
	default:
		return false
	}
}

func validRisk(risk RiskClass) bool {
	switch risk {
	case RiskReadOnly, RiskEffectful, RiskIrreversible:
		return true
	default:
		return false
	}
}

func SchemaDigest(schema []byte) string {
	sum := sha256.Sum256(schema)
	return hex.EncodeToString(sum[:])
}

func (d Descriptor) String() string {
	return fmt.Sprintf("%s@%s", d.Name, d.Version)
}

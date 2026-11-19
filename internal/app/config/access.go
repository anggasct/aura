package config

import (
	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/principal"
)

func validateAccess(section *principal.Config) error {
	if section == nil {
		return nil
	}
	return configkit.Under("access", section.Validate())
}

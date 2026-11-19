package config

import (
	"errors"
	"maps"
	"slices"

	"github.com/anggasct/aura/internal/platform/configkit"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/vision"
)

func validateVision(section *vision.Config) error {
	if section == nil {
		return nil
	}
	return errors.Join(
		configkit.Under("vision", section.Validate()),
		visionProviderProblems(section),
	)
}

func visionProviderProblems(section *vision.Config) error {
	if section == nil {
		return nil
	}
	var problems []error
	for _, name := range slices.Sorted(maps.Keys(section.Providers)) {
		limits := section.Providers[name]
		if !model.ValidProtocol(name) {
			problems = append(problems, configkit.Invalid("vision.providers."+name, "is not a known provider protocol"))
			continue
		}
		if limits.MaxImages < 0 || limits.MaxImages > section.MaxImages {
			problems = append(problems, configkit.Invalid("vision.providers."+name+".max_images", "must be between 0 and vision.max_images"))
		}
		if limits.MaxRequestBytes < 0 || limits.MaxRequestBytes > section.MaxRequestBytes {
			problems = append(problems, configkit.Invalid("vision.providers."+name+".max_request_bytes", "must be between 0 and vision.max_request_bytes"))
		}
		if limits.Detail != "" && !vision.ValidDetail(limits.Detail) {
			problems = append(problems, configkit.Invalid("vision.providers."+name+".detail", "must be auto, low, or high"))
		}
	}
	return errors.Join(problems...)
}

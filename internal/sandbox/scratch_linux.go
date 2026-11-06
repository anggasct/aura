//go:build linux

package sandbox

import (
	"os"
	"slices"
)

const scratchPattern = "aura-scratch-"

func withScratch(spec *Spec) (*Spec, string, error) {
	if !spec.Profile.scratchDir() {
		return spec, "", nil
	}
	dir, err := os.MkdirTemp("", scratchPattern)
	if err != nil {
		return nil, "", Errorf(ErrorCodeSandboxInitFailed, "create scratch directory: %v", pathlessCause(err))
	}
	scoped := *spec
	scoped.ReadOnlyPaths = append(slices.Clone(spec.ReadOnlyPaths), spec.WorkingDir)
	scoped.ReadWritePaths = append(slices.Clone(spec.ReadWritePaths), dir)
	scoped.AllowEnv = append(slices.Clone(spec.AllowEnv), "TMPDIR="+dir)
	return &scoped, dir, nil
}

func removeScratch(dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

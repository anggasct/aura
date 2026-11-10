package interpreter

import (
	"maps"
	"runtime"

	"github.com/anggasct/aura/internal/platform/capability"
)

const (
	SupportedOS   = "linux"
	SupportedArch = "amd64"
)

func Supported(goos, goarch string) bool {
	return goos == SupportedOS && goarch == SupportedArch
}

func HostSupported() bool {
	return Supported(runtime.GOOS, runtime.GOARCH)
}

func Dependencies(goos, goarch string, base capability.Dependencies) capability.Dependencies {
	out := make(capability.Dependencies, len(base)+1)
	maps.Copy(out, base)
	if Supported(goos, goarch) && base[capability.DependencyProcessContainment] {
		out[capability.DependencyInterpreterRuntime] = true
	}
	return out
}

func HostDependencies(base capability.Dependencies) capability.Dependencies {
	return Dependencies(runtime.GOOS, runtime.GOARCH, base)
}

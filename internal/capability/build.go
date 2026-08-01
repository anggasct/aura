package capability

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
)

type Profile string

const (
	ProfileCore         Profile = "core"
	ProfileExecLinux    Profile = "exec-linux"
	ProfileBrowserLinux Profile = "browser-linux"
	ProfileNativeLinux  Profile = "native-linux"
)

var (
	buildProfile      = string(ProfileCore)
	buildCapabilities string
)

type Build struct {
	profile      Profile
	capabilities []string
	goos         string
}

func CurrentBuild() (Build, error) {
	return ParseBuild(buildProfile, buildCapabilities, runtime.GOOS)
}

func ParseBuild(profile, capabilities, goos string) (Build, error) {
	var compiled []string
	if capabilities != "" {
		compiled = strings.Split(capabilities, ",")
	}
	return NewBuild(profile, compiled, goos)
}

func NewBuild(profile string, capabilities []string, goos string) (Build, error) {
	p := Profile(profile)
	if !p.Valid() {
		return Build{}, newError(ErrorCodeProfileInvalid, "", fmt.Sprintf("unsupported build profile %q", profile))
	}
	if goos == "" {
		return Build{}, newError(ErrorCodeProfileInvalid, "", "build operating system is empty")
	}
	if p.requiresLinux() && goos != "linux" {
		return Build{}, newError(ErrorCodeCapabilityUnavailable, "", fmt.Sprintf("%q requires Linux", p))
	}

	compiled := slices.Clone(capabilities)
	seen := make(map[string]struct{}, len(compiled))
	for _, name := range compiled {
		if name == "" || strings.TrimSpace(name) != name || !namePattern.MatchString(name) {
			return Build{}, newError(ErrorCodeProfileInvalid, name, "compiled capability name is invalid")
		}
		if _, ok := seen[name]; ok {
			return Build{}, newError(ErrorCodeProfileInvalid, name, "compiled capability is duplicated")
		}
		seen[name] = struct{}{}
	}
	slices.Sort(compiled)

	return Build{profile: p, capabilities: compiled, goos: goos}, nil
}

func SupportedProfiles() []Profile {
	return []Profile{ProfileCore, ProfileExecLinux, ProfileBrowserLinux, ProfileNativeLinux}
}

func (p Profile) Valid() bool {
	return slices.Contains(SupportedProfiles(), p)
}

func (p Profile) requiresLinux() bool {
	return p == ProfileExecLinux || p == ProfileBrowserLinux || p == ProfileNativeLinux
}

func (b Build) Profile() Profile {
	return b.profile
}

func (b Build) CompiledCapabilities() []string {
	return slices.Clone(b.capabilities)
}

func (b Build) GOOS() string {
	return b.goos
}

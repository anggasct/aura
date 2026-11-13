package capability

const (
	DependencyProcessContainment = "process-containment"
	DependencyInterpreterRuntime = "interpreter-runtime"
)

func BuiltinRegistry() (Registry, error) {
	return NewRegistry(
		Definition{
			Name:       "exec-linux",
			Effectful:  true,
			Profiles:   []Profile{ProfileExecLinux, ProfileNativeLinux},
			Dependency: DependencyProcessContainment,
		},
		Definition{
			Name:       "code-interpreter",
			Effectful:  true,
			Profiles:   []Profile{ProfileExecLinux, ProfileNativeLinux},
			Dependency: DependencyInterpreterRuntime,
		},
		Definition{
			Name:     "workspace-read",
			Profiles: []Profile{ProfileExecLinux, ProfileNativeLinux},
		},
		Definition{
			Name:      "workspace-write",
			Effectful: true,
			Profiles:  []Profile{ProfileExecLinux, ProfileNativeLinux},
		},
		Definition{
			Name:     "public-web",
			Profiles: []Profile{ProfileCore, ProfileExecLinux, ProfileBrowserLinux, ProfileNativeLinux},
		},
		Definition{
			Name:     "provider-search",
			Profiles: []Profile{ProfileCore, ProfileExecLinux, ProfileBrowserLinux, ProfileNativeLinux},
		},
		Definition{
			Name:     "provider-github",
			Profiles: []Profile{ProfileCore, ProfileExecLinux, ProfileBrowserLinux, ProfileNativeLinux},
		},
	)
}

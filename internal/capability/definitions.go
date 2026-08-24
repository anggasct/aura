package capability

// DependencyProcessContainment names the host containment contract the
// effectful execution capability requires.
const DependencyProcessContainment = "process-containment"

// BuiltinRegistry returns the capability set this binary ships: the
// capabilities the built-in tool layer admits. Network capabilities are
// profile-independent; filesystem and process capabilities are bound to
// the Linux containment profiles.
func BuiltinRegistry() (Registry, error) {
	return NewRegistry(
		Definition{
			Name:       "exec-linux",
			Effectful:  true,
			Profiles:   []Profile{ProfileExecLinux, ProfileNativeLinux},
			Dependency: DependencyProcessContainment,
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
	)
}

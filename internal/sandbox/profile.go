package sandbox

type Profile string

const (
	ProfileBase      Profile = ""
	ProfileConnector Profile = "connector"
)

func (p Profile) validate() error {
	switch p {
	case ProfileBase, ProfileConnector:
		return nil
	default:
		return Errorf(ErrorCodeInvalidArgument, "unknown sandbox profile")
	}
}

func (p Profile) workingDirReadOnly() bool {
	return p == ProfileConnector
}

func (p Profile) scratchDir() bool {
	return p == ProfileConnector
}

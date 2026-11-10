package sandbox

import "time"

type Profile string

const (
	ProfileBase        Profile = ""
	ProfileConnector   Profile = "connector"
	ProfileInterpreter Profile = "interpreter"
)

func (p Profile) validate() error {
	switch p {
	case ProfileBase, ProfileConnector, ProfileInterpreter:
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

func (p Profile) requireLimits(limits *Limits) error {
	if p != ProfileInterpreter {
		return nil
	}
	switch {
	case limits.MemoryBytes <= 0:
		return Errorf(ErrorCodeInvalidArgument, "interpreter profile requires a memory limit")
	case limits.MaxProcesses <= 0:
		return Errorf(ErrorCodeInvalidArgument, "interpreter profile requires a process limit")
	case limits.CPUTime < time.Second:
		return Errorf(ErrorCodeInvalidArgument, "interpreter profile requires a cpu limit of at least one second")
	case limits.FileBytes <= 0:
		return Errorf(ErrorCodeInvalidArgument, "interpreter profile requires a file size limit")
	case limits.MaxOpenFiles <= 0:
		return Errorf(ErrorCodeInvalidArgument, "interpreter profile requires an open file limit")
	case limits.MaxOutputBytes <= 0:
		return Errorf(ErrorCodeInvalidArgument, "interpreter profile requires an output limit")
	case limits.RunDirBytes <= 0:
		return Errorf(ErrorCodeInvalidArgument, "interpreter profile requires a run directory limit")
	default:
		return nil
	}
}

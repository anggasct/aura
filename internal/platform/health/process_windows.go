//go:build windows

package health

func ProcessProbe(func() (string, bool)) (ProcessStatus, bool) {
	return ProcessStatus{}, false
}

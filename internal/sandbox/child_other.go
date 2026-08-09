//go:build !linux

package sandbox

// RunChild is unreachable on non-Linux: the non-Linux run path fails closed
// before spawning a child, so the sentinel dispatch never fires. It returns
// a non-zero exit if ever invoked.
func RunChild() int { return 1 }

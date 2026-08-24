//go:build windows

package cli

import "github.com/anggasct/aura/internal/health"

// processProbe has no portable limit surface on this platform; the checker
// emits no findings rather than guessing.
func processProbe() (health.ProcessStatus, bool) {
	return health.ProcessStatus{}, false
}

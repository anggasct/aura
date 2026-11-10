//go:build windows

package cli

import (
	"github.com/anggasct/aura/internal/platform/health"
)

func processProbe() (health.ProcessStatus, bool) {
	return health.ProcessStatus{}, false
}

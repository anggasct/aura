//go:build windows

package app

import (
	"github.com/anggasct/aura/internal/platform/health"
)

func processProbe() (health.ProcessStatus, bool) {
	return health.ProcessStatus{}, false
}

package codeexec

import (
	"context"
	"strings"

	"github.com/anggasct/aura/internal/sandbox"
	"github.com/anggasct/aura/internal/tools/interpreter"
)

func (r *Runner) Probe(ctx context.Context, bundle *interpreter.Bundle) error {
	language, supported := languageFor(bundle.Runtime.ID)
	if !supported {
		return interpreter.ErrProbeUnsupported
	}
	outcome, err := r.run(ctx, &plan{
		bundle: *bundle, language: language, source: language.probe, cpu: probeTimeout,
		limits: sandbox.Limits{
			MemoryBytes: probeMemoryBytes, CPUTime: probeTimeout, MaxOutputBytes: probeOutputBytes, MaxOpenFiles: openFilesLimit,
			MaxProcesses: probeProcesses, FileBytes: probeRunDirBytes, Timeout: probeTimeout, NoProcessRlimit: true, RunDirBytes: probeRunDirBytes,
		},
	})
	if err != nil {
		return err
	}
	if outcome.Class != ClassOK || strings.TrimSpace(outcome.Stdout) != coreVersion(bundle.Runtime.Version) {
		return Errorf(ErrorCodeUnavailable, "the %s version probe did not report the pinned version", bundle.Runtime.ID)
	}
	return nil
}

var _ interpreter.Prober = (*Runner)(nil)

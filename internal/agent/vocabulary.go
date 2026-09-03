package agent

import "slices"

// Capability names form their own namespace: they describe what a definition
// is for, never what it may do. Tool invocation still passes the broker
// policy, approval, build-capability, and sandbox gates, so a declared
// capability grants nothing by itself.
const (
	CapabilityRepositoryRead    = "repository.read"
	CapabilityRepositoryWrite   = "repository.write"
	CapabilityShellExecute      = "shell.execute"
	CapabilityGitDiff           = "git.diff"
	CapabilityCodeReview        = "code.review"
	CapabilityWebSearch         = "web.search"
	CapabilityWebRead           = "web.read"
	CapabilityDocumentRead      = "document.read"
	CapabilityObservabilityRead = "observability.read"
)

var knownCapabilities = []string{
	CapabilityRepositoryRead,
	CapabilityRepositoryWrite,
	CapabilityShellExecute,
	CapabilityGitDiff,
	CapabilityCodeReview,
	CapabilityWebSearch,
	CapabilityWebRead,
	CapabilityDocumentRead,
	CapabilityObservabilityRead,
}

func knownCapability(name string) bool {
	return slices.Contains(knownCapabilities, name)
}

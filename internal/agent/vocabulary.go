package agent

import "slices"

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

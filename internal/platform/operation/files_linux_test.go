//go:build linux

package operation

import "path/filepath"

func workspaceAuthorityCases(workspace string, agent *surfaceFiles) []fileAuthorityCase {
	return []fileAuthorityCase{
		{name: "a workspace file", files: agent, source: "policy.md", wantName: "policy.md"},
		{name: "an escaping path", files: agent, source: "../policy.md", wantCode: ErrorCodePolicyDenied},
		{name: "an absolute path", files: agent, source: filepath.Join(workspace, "policy.md"), wantCode: ErrorCodePolicyDenied},
		{name: "a missing file", files: agent, source: "missing.md", wantCode: ErrorCodeSourceUnavailable},
		{name: "a directory", files: agent, source: "directory", wantCode: ErrorCodeSourceUnavailable},
	}
}

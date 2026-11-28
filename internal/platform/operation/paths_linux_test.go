//go:build linux

package operation_test

import "github.com/anggasct/aura/internal/platform/operation"

func workspacePathResolutionCases() []pathResolutionCase {
	return []pathResolutionCase{
		{name: "a workspace path with its scheme on the CLI", surface: operation.SurfaceCLI, source: "workspace:policy.md", wantName: "policy.md", wantRef: "workspace:policy.md", wantBody: "workspace body", wantInput: map[string]string{}},
	}
}

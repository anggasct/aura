package workspaceops

import (
	"github.com/anggasct/aura/internal/platform/operation"
)

const (
	GroupName        = "workspace"
	operationUntaint = "workspace.untaint"
	maxPathBytes     = 512
)

func Catalog() operation.Catalog {
	return operation.Catalog{
		Group: operation.Group{
			Name:    GroupName,
			Summary: "Inspect and clear workspace taint records",
		},
		Descriptors: []operation.Descriptor{
			{
				ID:            operationUntaint,
				Summary:       "Preview or remove workspace taint records",
				Description:   "Lists workspace taint records (path, short digest, source and date) and prints a confirmation code. With --apply <code>, removes the listed records.",
				Exposure:      operation.ExposureHost,
				Risk:          operation.RiskTrustGrant,
				Commit:        operation.CommitTransactional,
				Surfaces:      []operation.Surface{operation.SurfaceCLI},
				SurfaceReason: "only the host CLI clears workspace taint records",
				Preview:       true,
				Confirmation:  operation.ConfirmPreviewWithCode,
				Params: []operation.Param{
					{
						Name:      "path",
						Kind:      operation.KindString,
						Summary:   "only records for this workspace-relative path",
						Position:  1,
						MinLength: 1,
						MaxLength: maxPathBytes,
					},
				},
				Output: operation.Output{Kind: operation.OutputText},
			},
		},
	}
}

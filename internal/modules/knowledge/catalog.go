package knowledge

import (
	"github.com/anggasct/aura/internal/platform/operation"
)

const (
	GroupName = "kb"

	operationAdd     = "kb.add"
	operationCheck   = "kb.check"
	operationEmbed   = "kb.embed"
	operationList    = "kb.list"
	operationRefresh = "kb.refresh"
	operationRemove  = "kb.remove"
	operationShow    = "kb.show"
	operationTrust   = "kb.trust"
	operationUnembed = "kb.unembed"
	operationUntrust = "kb.untrust"

	maxCollectionIDBytes = 48
	maxSourceInputBytes  = 4096
)

func collectionParam() operation.Param {
	return operation.Param{
		Name: "collection", Kind: operation.KindString, Summary: "collection id", Required: true, Position: 1,
		MinLength: 1, MaxLength: maxCollectionIDBytes,
	}
}

func storedSourceParam(required bool) operation.Param {
	return operation.Param{
		Name: "source", Kind: operation.KindString, Summary: "stored source uri as kb show lists it", Required: required, Position: 2,
		MinLength: 1, MaxLength: maxSourceInputBytes,
	}
}

func textOutput() operation.Output {
	return operation.Output{Kind: operation.OutputText}
}

func addParams() []operation.Param {
	return []operation.Param{
		collectionParam(),
		{
			Name: "source", Kind: operation.KindPath, Position: 2, Required: true, URLParam: "url",
			Summary:  "a file or an https URL",
			Surfaces: []operation.Surface{operation.SurfaceCLI, operation.SurfaceAgent},
			Texts: []operation.SurfaceText{
				{Surface: operation.SurfaceCLI, Text: "a file on this host or an https URL"},
				{Surface: operation.SurfaceAgent, Text: "a file path in the workspace, artifact:<id> of an attachment of this session, or an https URL"},
			},
		},
		{
			Name: "url", Kind: operation.KindString, Summary: "https URL of the document", Position: 3, Required: true,
			MinLength: 1, MaxLength: maxURIBytes, Surfaces: []operation.Surface{operation.SurfaceChat},
		},
	}
}

func Catalog() operation.Catalog {
	needs := []string{"knowledge"}
	return operation.Catalog{
		Group: operation.Group{Name: GroupName, Summary: "Manage knowledge collections"},
		Descriptors: []operation.Descriptor{
			{
				ID: operationList, Summary: "List collections that have sources", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone, Needs: needs,
				Output: operation.Output{Kind: operation.OutputTable, Fields: []operation.OutputField{
					{Name: "collection"}, {Name: "sources"}, {Name: "chunks"}, {Name: "generation"}, {Name: "trust"}, {Name: "trusted_at"},
				}},
			},
			{
				ID: operationShow, Summary: "Show the sources of a collection", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone, UntrustedOutput: true, Needs: needs,
				Description: "Source labels and addresses come from documents and pages, not from the owner.",
				Params:      []operation.Param{collectionParam()},
				Output: operation.Output{
					Kind:   operation.OutputTable,
					Header: []operation.OutputField{{Name: "collection"}, {Name: "sources"}, {Name: "chunks"}, {Name: "trust"}, {Name: "trusted_at"}},
					Fields: []operation.OutputField{
						{Name: "uri"}, {Name: "label"}, {Name: "kind"}, {Name: "chunks"}, {Name: "digest"}, {Name: "ingested"}, {Name: "vouched"},
					},
				},
			},
			{
				ID: operationCheck, Summary: "Verify the knowledge index against its content", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone, Needs: needs,
				Output: operation.Output{Kind: operation.OutputRecord, Fields: []operation.OutputField{
					{Name: "collections"}, {Name: "sources"}, {Name: "chunks"}, {Name: "indexed_chunks"}, {Name: "status"},
				}},
			},
			{
				ID: operationAdd, Summary: "Ingest a document into a collection", Exposure: operation.ExposureAgent,
				Risk: operation.RiskChange, Commit: operation.CommitTransactional, Sink: true, Preview: true, Confirmation: operation.ConfirmRunDirectly, ReachesNetwork: true, Needs: needs,
				Description: "Reads Markdown, plain text or HTML from an https URL and stores its text as searchable chunks. An address you choose is fetched from this host.",
				Texts: []operation.SurfaceText{
					{Surface: operation.SurfaceCLI, Text: "Reads Markdown, plain text or HTML from an https URL or a file on this host and stores its text as searchable chunks. An address you choose is fetched from this host."},
					{Surface: operation.SurfaceAgent, Text: "Reads Markdown, plain text or HTML from an https URL, a file in the workspace or an attachment of this session and stores its text as searchable chunks. An address you choose is fetched from this host."},
				},
				Params: addParams(), Output: textOutput(),
			},
			{
				ID: operationRefresh, Summary: "Re-read the stored sources of a collection", Exposure: operation.ExposureAgent,
				Risk: operation.RiskChange, Commit: operation.CommitTransactional, ReachesNetwork: true, Needs: needs,
				Description: "Reads again the sources already stored in the collection. A source that cannot be read keeps its stored chunks and is reported; a stored attachment never changes; a workspace file is refreshed from the CLI or the agent and a file on the host only from the CLI.",
				Params:      []operation.Param{collectionParam(), storedSourceParam(false)}, Output: textOutput(),
			},
			{
				ID: operationRemove, Summary: "Remove a source and its chunks from a collection", Exposure: operation.ExposureAgent,
				Risk: operation.RiskDestructive, Commit: operation.CommitTransactional, Preview: true, Needs: needs,
				Params: []operation.Param{collectionParam(), storedSourceParam(true)}, Output: textOutput(),
			},
			{
				ID: operationEmbed, Summary: "Send a collection's chunks to the embedding provider for vectors", Exposure: operation.ExposureAgent,
				Risk: operation.RiskTrustGrant, Commit: operation.CommitExternal, Preview: true, Confirmation: operation.ConfirmPreviewUntilApply, ReachesNetwork: true, Needs: needs,
				TaintedGrant: operation.TaintedGrantInert,
				Description:  "Opts the collection in to sending its chunk text to the configured embedding provider and stores the vectors; it also rebuilds vectors after the model or destination changed.",
				Params:       []operation.Param{collectionParam()}, Output: textOutput(),
			},
			{
				ID: operationUnembed, Summary: "Clear a collection's embedding opt-in and erase its vectors", Exposure: operation.ExposureAgent,
				Risk: operation.RiskChange, Commit: operation.CommitTransactional, Preview: true, Confirmation: operation.ConfirmPreviewUntilApply, Needs: needs,
				Params: []operation.Param{collectionParam()}, Output: textOutput(),
			},
			{
				ID: operationTrust, Summary: "Mark a collection trusted and vouch for its current sources", Exposure: operation.ExposureHost,
				Risk: operation.RiskTrustGrant, Commit: operation.CommitTransactional,
				Surfaces:      []operation.Surface{operation.SurfaceCLI},
				SurfaceReason: "only the host CLI trusts a knowledge collection",
				Preview:       true, Confirmation: operation.ConfirmPreviewWithCode, Needs: needs,
				Description: "Previews the collection and its current sources with a confirmation code. With --apply <code>, marks the collection trusted and vouches every current source at its current content digest.",
				Params:      []operation.Param{collectionParam()}, Output: textOutput(),
			},
			{
				ID: operationUntrust, Summary: "Mark a collection untrusted and clear its sources' vouching", Exposure: operation.ExposureOwner,
				Risk: operation.RiskChange, Commit: operation.CommitTransactional, Needs: needs,
				Description: "Clears the collection's trusted state and every source's vouched digest so its passages mark conversations as carrying untrusted content again.",
				Params:      []operation.Param{collectionParam()}, Output: textOutput(),
			},
		},
	}
}

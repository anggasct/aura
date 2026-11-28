package memory

import (
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/operation"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

const (
	GroupName = "memory"

	operationInspect = "memory.inspect"
	operationForget  = "memory.forget"
	operationPurge   = "memory.purge"

	maxSessionIDBytes   = 512
	maxDocumentIDBytes  = 128
	defaultInspectLimit = "50"
	defaultForgetLimit  = "100"
	defaultPurgeLimit   = "100"
)

func limitParam(summary string, maximum int64, fallback string) operation.Param {
	one := int64(1)
	return operation.Param{
		Name: "limit", Kind: operation.KindInteger, Summary: summary,
		Minimum: &one, Maximum: &maximum, Default: json.RawMessage(fallback),
	}
}

func sessionParam(summary string) operation.Param {
	return operation.Param{Name: "session", Kind: operation.KindString, Summary: summary, MinLength: 1, MaxLength: maxSessionIDBytes}
}

func queryParam(summary string) operation.Param {
	return operation.Param{Name: "query", Kind: operation.KindString, Summary: summary, MinLength: 1, MaxLength: runtimeingress.MaxQueryRunes}
}

func textOutput() operation.Output {
	return operation.Output{Kind: operation.OutputText}
}

func Catalog() operation.Catalog {
	needs := []string{"memory"}
	return operation.Catalog{
		Group: operation.Group{Name: GroupName, Summary: "Inspect and forget projected session memory"},
		Descriptors: []operation.Descriptor{
			{
				ID: operationInspect, Summary: "List recall documents with their provenance", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone, UntrustedOutput: true, Needs: needs,
				Description: "Lists what Aura remembers from past conversations, with session, range, trust label and expiry. The content comes from conversations and tool results, not from the owner alone.",
				Params: []operation.Param{
					sessionParam("only the documents of this session id"),
					queryParam("only the documents this text finds"),
					limitParam("maximum documents to list", maxInspectLimit, defaultInspectLimit),
				},
				Output: textOutput(),
			},
			{
				ID: operationForget, Summary: "Preview or delete recall documents", Exposure: operation.ExposureAgent,
				Risk: operation.RiskDestructive, Commit: operation.CommitTransactional, Preview: true, Needs: needs,
				Description: "Deletes recall documents of one session, one document by id, or the ones a query finds; exactly one of session, id and query is required. The preview lists the documents that would be deleted and the delete removes exactly those. Conversation history is not touched.",
				Params: []operation.Param{
					sessionParam("forget every document of this session id"),
					{Name: "id", Kind: operation.KindString, Summary: "forget this document id", MinLength: 1, MaxLength: maxDocumentIDBytes},
					queryParam("forget the documents this text finds"),
					limitParam("maximum documents in the preview and the delete", maxForgetLimit, defaultForgetLimit),
				},
				Output: textOutput(),
			},
			{
				ID: operationPurge, Summary: "Preview or delete expired summary documents", Exposure: operation.ExposureAgent,
				Risk: operation.RiskDestructive, Commit: operation.CommitTransactional, Preview: true, Needs: needs,
				Description: "Deletes summary documents past their expiry, the cleanup the server also runs on a timer. The preview lists the documents that would be deleted and the delete removes exactly those.",
				Params:      []operation.Param{limitParam("maximum expired documents to delete", maxPurgeLimit, defaultPurgeLimit)},
				Output:      textOutput(),
			},
		},
	}
}

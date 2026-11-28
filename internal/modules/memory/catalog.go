package memory

import (
	"encoding/json"

	"github.com/anggasct/aura/internal/platform/operation"
	runtimeingress "github.com/anggasct/aura/internal/platform/turn/ingress"
)

const (
	GroupName = "memory"

	operationInspect = "memory.inspect"
	operationSearch  = "memory.search"
	operationForget  = "memory.forget"
	operationPurge   = "memory.purge"

	maxSessionIDBytes   = 512
	maxDocumentIDBytes  = 128
	maxTimestampBytes   = 64
	maxSearchLimit      = 20
	defaultInspectLimit = "50"
	defaultSearchLimit  = "10"
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

func inspectOutput() operation.Output {
	names := []string{"id", "agent", "session", "kind", "from", "to", "trust", "tainted", "prompt", "expires", "content"}
	fields := make([]operation.OutputField, 0, len(names))
	for _, name := range names {
		fields = append(fields, operation.OutputField{Name: name})
	}
	return operation.Output{Kind: operation.OutputTable, Fields: fields}
}

func searchOutput() operation.Output {
	names := []string{"id", "agent", "session", "kind", "created_at", "score", "tainted", "content"}
	fields := make([]operation.OutputField, 0, len(names))
	for _, name := range names {
		fields = append(fields, operation.OutputField{Name: name})
	}
	return operation.Output{Kind: operation.OutputTable, Fields: fields}
}

func memoryRowsTaint(raw json.RawMessage) bool {
	var parsed struct {
		Rows []struct {
			Tainted *bool `json:"tainted"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Rows == nil {
		return true
	}
	for i := range parsed.Rows {
		if parsed.Rows[i].Tainted == nil || *parsed.Rows[i].Tainted {
			return true
		}
	}
	return false
}

func Catalog() operation.Catalog {
	needs := []string{"memory"}
	return operation.Catalog{
		Group: operation.Group{Name: GroupName, Summary: "Inspect and forget projected session memory"},
		Descriptors: []operation.Descriptor{
			{
				ID: operationInspect, Summary: "List recall documents with their provenance", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone, ResultTaint: memoryRowsTaint, Needs: needs,
				Description: "Lists what Aura remembers from past conversations across every persona, with session, range, trust label and expiry. The content comes from conversations and tool results, not from the owner alone.",
				Texts: []operation.SurfaceText{
					{Surface: operation.SurfaceAgent, Text: "Lists what your persona remembers from past conversations, with session, range, trust label and expiry; memory of another persona is never listed. The content comes from conversations and tool results, not from the owner alone."},
				},
				Params: []operation.Param{
					sessionParam("only the documents of this session id"),
					queryParam("only the documents this text finds"),
					limitParam("maximum documents to list", maxInspectLimit, defaultInspectLimit),
				},
				Output: inspectOutput(),
			},
			{
				ID: operationSearch, Summary: "Search recall memory by query and time range", Exposure: operation.ExposureAgent,
				Risk: operation.RiskRead, Commit: operation.CommitNone, ResultTaint: memoryRowsTaint, Needs: needs,
				Description: "Searches projected session memory across every persona using ranked lexical recall with recency weighting and optional time bounds. Results come from past conversations and tool outputs.",
				Texts: []operation.SurfaceText{
					{Surface: operation.SurfaceAgent, Text: "Searches projected session memory within your persona's readable memory scope using ranked lexical recall with recency weighting and optional time bounds. Memory of private other personas is never returned."},
				},
				Params: []operation.Param{
					{Name: "query", Kind: operation.KindString, Summary: "search terms to match in memory", Required: true, Position: 1, MinLength: 1, MaxLength: runtimeingress.MaxQueryRunes},
					sessionParam("only search documents of this session id"),
					{Name: "after", Kind: operation.KindString, Summary: "only documents created at or after this RFC3339 timestamp", MinLength: 1, MaxLength: maxTimestampBytes},
					{Name: "before", Kind: operation.KindString, Summary: "only documents created at or before this RFC3339 timestamp", MinLength: 1, MaxLength: maxTimestampBytes},
					limitParam("maximum matching documents to return", maxSearchLimit, defaultSearchLimit),
				},
				Output: searchOutput(),
			},
			{
				ID: operationForget, Summary: "Preview or delete recall documents", Exposure: operation.ExposureAgent,
				Risk: operation.RiskDestructive, Commit: operation.CommitTransactional, Preview: true, Needs: needs,
				Description: "Deletes recall documents of one session, one document by id, or the ones a query finds, across every persona; exactly one of session, id and query is required. The preview lists the documents that would be deleted and the delete removes exactly those. Conversation history is not touched.",
				Texts: []operation.SurfaceText{
					{Surface: operation.SurfaceAgent, Text: "Deletes recall documents of your own persona: of one session, one document by id, or the ones a query finds; exactly one of session, id and query is required. The owner sees the documents that would be deleted on a card and the delete removes exactly those. The card lists up to 20 documents with their session and persona, and a card that cannot list every document is refused instead of shortened: set limit to a smaller number and delete in batches. Memory of another persona is out of reach and conversation history is not touched."},
				},
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
				Description: "Deletes summary documents whose retention period has already ended, for every persona; recall no longer returns them and the server runs the same cleanup on a timer. It deletes nothing that is still within its retention. The preview lists the documents that would be deleted and the delete removes exactly those.",
				Params:      []operation.Param{limitParam("maximum expired documents to delete", maxPurgeLimit, defaultPurgeLimit)},
				Output:      textOutput(),
			},
		},
	}
}

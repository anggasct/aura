package runtimeadk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anggasct/aura/internal/platform/durable"
	auramodel "github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/turn"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

const (
	CompactionItemEvent   = "event"
	CompactionItemSummary = "summary"
)

type ModelCapabilities struct {
	ID            string
	ContextTokens int
	Tokenizer     string
}

type CapabilityLookup interface {
	ModelCapabilities(name string) (ModelCapabilities, bool)
}

type CompactionRequest struct {
	SessionID       string
	TurnID          string
	InvocationID    string
	Capability      ModelCapabilities
	SystemText      string
	ToolText        string
	MaxOutputTokens int
	PriorEvents     []turn.Event
	CurrentEvents   []turn.Event
}

type CompactionToolProjection struct {
	PartIndex   int    `json:"part_index"`
	Digest      string `json:"digest"`
	Bytes       int    `json:"bytes"`
	Excerpt     string `json:"excerpt"`
	RefSequence uint64 `json:"ref_sequence"`
	MediaType   string `json:"media_type,omitempty"`
}

type CompactionItem struct {
	Kind        string                     `json:"kind"`
	Sequence    uint64                     `json:"sequence,omitempty"`
	Projections []CompactionToolProjection `json:"projections,omitempty"`
	SummaryID   string                     `json:"summary_id,omitempty"`
	SummaryText string                     `json:"summary_text,omitempty"`
}

type CompactionResult struct {
	Items      []CompactionItem `json:"items"`
	Compacted  bool             `json:"compacted"`
	UsedTokens int              `json:"used_tokens"`
	Effective  int              `json:"effective"`
}

type HistoryCompactor interface {
	Compact(ctx context.Context, req *CompactionRequest) (*CompactionResult, error)
}

func WithHistoryCompactor(c HistoryCompactor) ExecutorOption {
	return func(e *ADKExecutor) error {
		if c == nil {
			return invalidArgument("history compactor must not be nil")
		}
		e.compactor = c
		return nil
	}
}

func WithCapabilityLookup(lookup CapabilityLookup) ExecutorOption {
	return func(e *ADKExecutor) error {
		if lookup == nil {
			return invalidArgument("capability lookup must not be nil")
		}
		e.capabilities = lookup
		return nil
	}
}

type sessionCompaction struct {
	compactor        HistoryCompactor
	turnID           string
	capability       ModelCapabilities
	systemText       string
	coreTools        []tool.Tool
	operationPlan    *operationPlan
	operationBinding *operationBinding
}

func compactWithJournal(
	ctx context.Context,
	cfg *sessionCompaction,
	sessionID string,
	step uint64,
	prior []turn.Event,
	current []turn.Event,
	toolEvents []*session.Event,
) (*CompactionResult, error) {
	if cfg == nil || cfg.compactor == nil {
		return nil, invalidArgument("session compaction is not configured")
	}
	run := func(runCtx context.Context) (*CompactionResult, error) {
		res, err := cfg.compactor.Compact(runCtx, &CompactionRequest{
			SessionID:       sessionID,
			TurnID:          cfg.turnID,
			InvocationID:    cfg.turnID,
			Capability:      cfg.capability,
			SystemText:      cfg.systemText,
			ToolText:        toolDefinitionsText(cfg.coreTools, cfg.operationPlan, cfg.operationBinding, toolEvents),
			MaxOutputTokens: 0,
			PriorEvents:     prior,
			CurrentEvents:   current,
		})
		if err != nil {
			return nil, err
		}
		if res != nil && res.Items == nil {
			res.Items = []CompactionItem{}
		}
		if err := validateCompactionResult(res, len(prior)); err != nil {
			return nil, err
		}
		return res, nil
	}
	scope, durableTurn := durable.TurnScopeFrom(ctx)
	if !durableTurn {
		return run(ctx)
	}
	key := fmt.Sprintf("compact-%d", step)
	raw, err := scope.Invocation().RunAction(ctx, key, func(actionCtx context.Context) ([]byte, error) {
		res, cerr := run(actionCtx)
		if cerr != nil {
			return nil, cerr
		}
		return json.Marshal(res)
	})
	if err != nil {
		return nil, err
	}
	var res *CompactionResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, codedError(turn.ErrorCodeRuntimeInternal, "decode journaled compaction result", err)
	}
	if err := validateCompactionResult(res, len(prior)); err != nil {
		return nil, err
	}
	return res, nil
}

func validateCompactionResult(res *CompactionResult, priorCount int) error {
	if res == nil {
		return codedError(turn.ErrorCodeRuntimeInternal, "journaled compaction result is nil", nil)
	}
	if res.Items == nil {
		return codedError(turn.ErrorCodeRuntimeInternal, "journaled compaction result misses items", nil)
	}
	if priorCount > 0 && len(res.Items) == 0 {
		return codedError(turn.ErrorCodeRuntimeInternal, "journaled compaction result dropped all prior history", nil)
	}
	return nil
}

func materializeCompactedEvents(res *CompactionResult, priorBySeq map[uint64]*session.Event) ([]*session.Event, error) {
	if err := validateCompactionResult(res, len(priorBySeq)); err != nil {
		return nil, err
	}
	out := make([]*session.Event, 0, len(res.Items))
	for i := range res.Items {
		item := &res.Items[i]
		switch item.Kind {
		case CompactionItemEvent:
			if item.Sequence == 0 {
				return nil, codedError(turn.ErrorCodeRuntimeInternal, "compacted event item carries no sequence", nil)
			}
			ev, ok := priorBySeq[item.Sequence]
			if !ok || ev == nil {
				return nil, codedError(turn.ErrorCodeRuntimeInternal, fmt.Sprintf("compacted event sequence %d missing from history", item.Sequence), nil)
			}
			if len(item.Projections) > 0 {
				cloned, err := cloneEventWithProjections(ev, item.Projections)
				if err != nil {
					return nil, err
				}
				out = append(out, cloned)
			} else {
				out = append(out, ev)
			}
		case CompactionItemSummary:
			if strings.TrimSpace(item.SummaryID) == "" || strings.TrimSpace(item.SummaryText) == "" {
				return nil, codedError(turn.ErrorCodeRuntimeInternal, "compacted summary item misses id or text", nil)
			}
			out = append(out, &session.Event{
				ID:           item.SummaryID,
				Author:       userAuthor,
				InvocationID: "compaction",
				LLMResponse: adkmodel.LLMResponse{
					Content: genai.NewContentFromText(item.SummaryText, genai.RoleUser),
				},
			})
		default:
			return nil, codedError(turn.ErrorCodeRuntimeInternal, fmt.Sprintf("unsupported compaction item kind %q", item.Kind), nil)
		}
	}
	return out, nil
}

func cloneEventWithProjections(ev *session.Event, projections []CompactionToolProjection) (*session.Event, error) {
	if len(projections) == 0 {
		return ev, nil
	}
	if ev == nil || ev.Content == nil {
		return nil, codedError(turn.ErrorCodeRuntimeInternal, "projected event carries no content", nil)
	}
	byPart := make(map[int]CompactionToolProjection, len(projections))
	for i := range projections {
		idx := projections[i].PartIndex
		if idx < 0 || idx >= len(ev.Content.Parts) {
			return nil, codedError(turn.ErrorCodeRuntimeInternal, fmt.Sprintf("projection part index %d out of range", idx), nil)
		}
		part := ev.Content.Parts[idx]
		if part == nil || part.FunctionResponse == nil {
			return nil, codedError(turn.ErrorCodeRuntimeInternal, fmt.Sprintf("projection part index %d is not a function response", idx), nil)
		}
		byPart[idx] = projections[i]
	}
	cloned := *ev
	contentCopy := *ev.Content
	partsCopy := make([]*genai.Part, len(ev.Content.Parts))
	for i, part := range ev.Content.Parts {
		if part == nil {
			continue
		}
		proj, hasProj := byPart[i]
		if !hasProj {
			partsCopy[i] = part
			continue
		}
		partCopy := *part
		respCopy := *part.FunctionResponse
		respCopy.Response = map[string]any{
			"projected":    true,
			"excerpt":      proj.Excerpt,
			"sha256":       proj.Digest,
			"bytes":        proj.Bytes,
			"ref_sequence": proj.RefSequence,
		}
		partCopy.FunctionResponse = &respCopy
		partsCopy[i] = &partCopy
	}
	contentCopy.Parts = partsCopy
	cloned.Content = &contentCopy
	return &cloned, nil
}

func hasPendingFunctionCall(events []*session.Event) bool {
	if len(events) == 0 {
		return false
	}
	pendingByID := make(map[string]int)
	pendingAnon := 0
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			if part == nil {
				continue
			}
			if part.FunctionCall != nil {
				if part.FunctionCall.ID != "" {
					pendingByID[part.FunctionCall.ID]++
				} else {
					pendingAnon++
				}
			}
			if part.FunctionResponse != nil {
				if part.FunctionResponse.ID != "" && pendingByID[part.FunctionResponse.ID] > 0 {
					pendingByID[part.FunctionResponse.ID]--
					if pendingByID[part.FunctionResponse.ID] == 0 {
						delete(pendingByID, part.FunctionResponse.ID)
					}
				} else if pendingAnon > 0 {
					pendingAnon--
				}
			}
		}
	}
	return len(pendingByID) > 0 || pendingAnon > 0
}

type declarationProvider interface {
	Declaration() *genai.FunctionDeclaration
}

func toolDefinitionsText(core []tool.Tool, plan *operationPlan, binding *operationBinding, appended []*session.Event) string {
	var all []tool.Tool
	all = append(all, core...)
	if plan != nil && plan.visible != nil && binding != nil && len(appended) > 0 {
		loaded := recordedLoads(appended, plan.visible)
		for _, name := range loaded {
			if found, ok := binding.tools[name]; ok {
				all = append(all, found)
			}
		}
	}
	if len(all) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, t := range all {
		if t == nil {
			continue
		}
		if dp, ok := t.(declarationProvider); ok {
			if decl := dp.Declaration(); decl != nil {
				if encoded, err := json.Marshal(decl); err == nil {
					sb.Write(encoded)
					sb.WriteByte('\n')
					continue
				}
			}
		}
		sb.WriteString(t.Name())
		if desc := t.Description(); desc != "" {
			sb.WriteByte(' ')
			sb.WriteString(desc)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func ephemeralBlocksText(blocks []turn.ContextBlock) string {
	var parts []string
	for i := range blocks {
		if blocks[i].Placement == turn.ContextPlacementEphemeral {
			if text := strings.TrimSpace(blocks[i].Content); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func (x *ADKExecutor) lookupCapabilities(modelName string, llm adkmodel.LLM) ModelCapabilities {
	if x.capabilities != nil {
		if caps, ok := x.capabilities.ModelCapabilities(modelName); ok {
			if caps.ID == "" {
				caps.ID = modelName
			}
			return caps
		}
	}
	if provider, ok := x.models.(CapabilityLookup); ok {
		if caps, found := provider.ModelCapabilities(modelName); found {
			if caps.ID == "" {
				caps.ID = modelName
			}
			return caps
		}
	}
	if provider, ok := x.models.(interface {
		Capabilities(string) (auramodel.Capabilities, bool)
	}); ok {
		if caps, found := provider.Capabilities(modelName); found {
			return ModelCapabilities{
				ID:            modelName,
				ContextTokens: caps.ContextTokens,
				Tokenizer:     caps.Tokenizer,
			}
		}
	}
	if llm != nil {
		if provider, ok := llm.(interface {
			Capabilities() (auramodel.Capabilities, bool)
		}); ok {
			if caps, found := provider.Capabilities(); found {
				return ModelCapabilities{
					ID:            modelName,
					ContextTokens: caps.ContextTokens,
					Tokenizer:     caps.Tokenizer,
				}
			}
		}
	}
	return ModelCapabilities{ID: modelName}
}

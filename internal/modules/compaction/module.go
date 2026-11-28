package compaction

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/model"
	"github.com/anggasct/aura/internal/platform/turn"
)

const (
	ItemKindEvent   = "event"
	ItemKindSummary = "summary"
)

type ModelSource interface {
	LLM(ctx context.Context, name string) (adkmodel.LLM, error)
}

type ModuleDeps struct {
	Config      Config
	Models      ModelSource
	ModelConfig model.Config
	ModelRoutes model.Routes
	Store       SummaryStore
	Summarizer  Summarizer
	Counter     TokenCounter
	Observer    Observer
	Logger      *slog.Logger
}

type Module struct {
	cfg          Config
	policy       *SelectionPolicy
	store        SummaryStore
	service      *Service
	promptDigest string
	counter      TokenCounter
	logger       *slog.Logger
}

type CompactInput struct {
	SessionID       string
	TurnID          string
	InvocationID    string
	Capability      Capability
	SystemText      string
	ToolText        string
	MaxOutputTokens int
	PriorEvents     []turn.Event
	CurrentEvents   []turn.Event
}

type ToolProjection struct {
	PartIndex  int
	Projection Projection
}

type CompactItem struct {
	Kind        string
	Sequence    uint64
	Projections []ToolProjection
	SummaryID   string
	SummaryText string
}

type CompactOutput struct {
	Items      []CompactItem
	Compacted  bool
	UsedTokens int
	Effective  int
}

func New(ctx context.Context, deps *ModuleDeps) (*Module, error) {
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if deps == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "compaction deps must not be nil")
	}
	if deps.Store == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "summary store must not be nil")
	}
	if err := deps.Config.Validate(); err != nil {
		return nil, err
	}
	prompt, err := PromptText(deps.Config.Summary.PromptVersion, deps.Config.Summary.MaxOutputTokens)
	if err != nil {
		return nil, err
	}
	policy, err := ResolveSelectionPolicyWithSummary(
		deps.Config.RecentCompleteTurns,
		deps.Config.HighWaterRatio,
		deps.Config.MaxToolExcerptBytes,
		deps.Config.Summary.MaxOutputTokens,
	)
	if err != nil {
		return nil, err
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	summarizer := deps.Summarizer
	if summarizer == nil && deps.Models != nil {
		producer, target, perr := ResolveProducerForTask(deps.Config.Summary.Task, deps.ModelConfig, deps.ModelRoutes)
		if perr != nil {
			logger.DebugContext(ctx, "context summarizer route unavailable", "component", "context", "task", deps.Config.Summary.Task, "error", perr)
		} else {
			llm, lerr := deps.Models.LLM(ctx, target)
			if lerr != nil {
				logger.DebugContext(ctx, "context summarizer model unavailable", "component", "context", "task", deps.Config.Summary.Task, "error", lerr)
			} else {
				built, serr := NewLLMSummarizer(llm, producer, deps.Config.Summary.MaxOutputTokens)
				if serr != nil {
					return nil, serr
				}
				summarizer = built
			}
		}
	}
	var service *Service
	if summarizer != nil {
		service, err = NewService(
			summarizer,
			deps.Store,
			deps.Config.Summary.Task,
			deps.Config.Summary.PromptVersion,
			deps.Config.Summary.MaxSourceTokens,
			deps.Config.Summary.MaxOutputTokens,
			deps.Counter,
			logger,
		)
		if err != nil {
			return nil, err
		}
		if deps.Observer != nil {
			service.WithObserver(deps.Observer)
		}
	}
	return &Module{
		cfg:          deps.Config,
		policy:       policy,
		store:        deps.Store,
		service:      service,
		promptDigest: promptDigestOf(prompt),
		counter:      deps.Counter,
		logger:       logger,
	}, nil
}

func (m *Module) Compact(ctx context.Context, in *CompactInput) (*CompactOutput, error) {
	if m == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "compaction module must not be nil")
	}
	if ctx == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "compaction input must not be nil")
	}
	currentInvID := "current:" + cmp.Or(in.InvocationID, in.TurnID, "turn")
	mapped := make([]Event, 0, len(in.PriorEvents)+len(in.CurrentEvents))
	priorRowBySeq := make(map[uint64]*turn.Event, len(in.PriorEvents))
	var maxPriorSeq uint64
	for i := range in.PriorEvents {
		row := &in.PriorEvents[i]
		ev := MapEvent(row)
		if ev.Sequence == 0 {
			return nil, fault.New(ErrorCodeInvalidArgument, "prior event carries no sequence")
		}
		if strings.TrimSpace(ev.TurnID) == "" {
			ev.TurnID = "seq-" + strconv.FormatUint(ev.Sequence, 10)
		}
		if ev.InvocationID == currentInvID {
			ev.InvocationID = "prior:" + ev.TurnID
		}
		priorRowBySeq[ev.Sequence] = row
		if ev.Sequence > maxPriorSeq {
			maxPriorSeq = ev.Sequence
		}
		mapped = append(mapped, ev)
	}
	for i := range in.CurrentEvents {
		row := &in.CurrentEvents[i]
		ev := MapEvent(row)
		ev.Sequence = maxPriorSeq + 1 + uint64(i)
		ev.InvocationID = currentInvID
		if strings.TrimSpace(ev.TurnID) == "" {
			ev.TurnID = cmp.Or(in.TurnID, "current")
		}
		mapped = append(mapped, ev)
	}
	counter := CounterForTokenizer(in.Capability.Tokenizer, m.counter)
	planner, err := NewPlanner(m.cfg.SafetyMarginTokens, m.cfg.ConservativeEstimatorRatio, counter)
	if err != nil {
		return nil, err
	}
	plan, err := planner.Plan(in.Capability, Invocation{
		SystemText:      in.SystemText,
		ToolText:        in.ToolText,
		MaxOutputTokens: in.MaxOutputTokens,
		InvocationID:    currentInvID,
	}, mapped)
	if err != nil {
		return nil, err
	}
	selection, err := Select(m.policy, plan, counter)
	if err != nil {
		return nil, err
	}
	if selection.Mode == SelectionNormal {
		items := make([]CompactItem, 0, len(in.PriorEvents))
		for i := range in.PriorEvents {
			items = append(items, CompactItem{Kind: ItemKindEvent, Sequence: in.PriorEvents[i].Sequence})
		}
		return &CompactOutput{
			Items:      items,
			Compacted:  false,
			UsedTokens: selection.UsedTokens,
			Effective:  plan.Budget.Effective,
		}, nil
	}
	return m.materializeEmergency(ctx, in, plan, selection, counter, priorRowBySeq)
}

type resolvedSummaryEntry struct {
	ID         string
	Text       string
	Tokens     int
	Degraded   bool
	StartSeq   uint64
	EndSeq     uint64
	EventCount int
}

type resolvedPart struct {
	Kind        PartKind
	GroupIndex  int
	Group       Group
	Protected   bool
	Projections map[uint64][]ToolProjection
	GroupTokens int
	Summaries   []resolvedSummaryEntry
}

func (m *Module) materializeEmergency(
	ctx context.Context,
	in *CompactInput,
	plan *Plan,
	selection *Selection,
	counter TokenCounter,
	priorRowBySeq map[uint64]*turn.Event,
) (*CompactOutput, error) {
	groups := copyGroups(plan.Groups)
	protected := markProtected(groups, m.cfg.RecentCompleteTurns)
	if _, err := projectOversizedToolResults(groups, protected, m.cfg.MaxToolExcerptBytes, counter); err != nil {
		return nil, err
	}
	kept := make(map[int]bool, len(groups))
	for pIdx := range selection.Parts {
		part := &selection.Parts[pIdx]
		if part.Kind != PartVerbatim {
			continue
		}
		for i := range groups {
			if groups[i].Kind == part.Group.Kind && groups[i].ID == part.Group.ID {
				kept[i] = true
				break
			}
		}
	}
	summaryCache := make(map[string][]resolvedSummaryEntry)
	for {
		parts, totalTokens, err := m.buildResolvedParts(ctx, in.SessionID, groups, protected, kept, counter, priorRowBySeq, summaryCache)
		if err != nil {
			return nil, err
		}
		if totalTokens <= plan.Budget.Effective {
			return collectCompactOutput(parts, priorRowBySeq, totalTokens, plan.Budget.Effective), nil
		}
		if oldest, ok := oldestOptionalKept(groups, protected, kept); ok {
			delete(kept, oldest)
			continue
		}
		if degraded, ok := degradeOldestSummary(parts, counter); ok {
			if degraded <= plan.Budget.Effective {
				return collectCompactOutput(parts, priorRowBySeq, degraded, plan.Budget.Effective), nil
			}
			for {
				next, ok := degradeOldestSummary(parts, counter)
				if !ok {
					break
				}
				if next <= plan.Budget.Effective {
					return collectCompactOutput(parts, priorRowBySeq, next, plan.Budget.Effective), nil
				}
			}
		}
		return nil, fault.New(ErrorCodeBudgetExceeded, "compacted context exceeds the validated budget")
	}
}

func (m *Module) buildResolvedParts(
	ctx context.Context,
	sessionID string,
	groups []Group,
	protected, kept map[int]bool,
	counter TokenCounter,
	priorRowBySeq map[uint64]*turn.Event,
	summaryCache map[string][]resolvedSummaryEntry,
) ([]resolvedPart, int, error) {
	var parts []resolvedPart
	totalTokens := 0
	for i := range groups {
		if kept[i] || protected[i] {
			projections, tokens, err := groupProjectionsAndTokens(&groups[i], protected[i], priorRowBySeq, m.cfg.MaxToolExcerptBytes, counter)
			if err != nil {
				return nil, 0, err
			}
			next, err := checkedAdd(totalTokens, tokens)
			if err != nil {
				return nil, 0, err
			}
			totalTokens = next
			parts = append(parts, resolvedPart{
				Kind:        PartVerbatim,
				GroupIndex:  i,
				Group:       groups[i],
				Protected:   protected[i],
				Projections: projections,
				GroupTokens: tokens,
			})
			continue
		}
		if i > 0 && !kept[i-1] && !protected[i-1] {
			continue
		}
		end := i
		for end+1 < len(groups) && !kept[end+1] && !protected[end+1] {
			end++
		}
		span := SummaryRange{StartSequence: groups[i].Events[0].Sequence}
		for j := i; j <= end; j++ {
			group := &groups[j]
			span.EndSequence = group.Events[len(group.Events)-1].Sequence
			span.Groups = append(span.Groups, GroupRef{Kind: group.Kind, ID: group.ID})
			span.EventCount += len(group.Events)
			span.Tokens += group.Tokens
		}
		cacheKey := fmt.Sprintf("%d-%d-%d", span.StartSequence, span.EndSequence, span.EventCount)
		entries, cached := summaryCache[cacheKey]
		if !cached {
			var err error
			entries, err = m.resolveRangeEntries(ctx, sessionID, span, groups, counter)
			if err != nil {
				return nil, 0, err
			}
			summaryCache[cacheKey] = entries
		}
		copied := append([]resolvedSummaryEntry(nil), entries...)
		for k := range copied {
			next, err := checkedAdd(totalTokens, copied[k].Tokens)
			if err != nil {
				return nil, 0, err
			}
			totalTokens = next
		}
		parts = append(parts, resolvedPart{
			Kind:      PartSummary,
			Summaries: copied,
		})
	}
	return parts, totalTokens, nil
}

func (m *Module) resolveRangeEntries(ctx context.Context, sessionID string, target SummaryRange, groups []Group, counter TokenCounter) ([]resolvedSummaryEntry, error) {
	events := RangeEvents(target, groups)
	if len(events) == 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "summary range matches no events")
	}
	chunks, err := splitEventsByTokens(events, counter, m.cfg.Summary.MaxSourceTokens, true)
	if err != nil {
		return nil, err
	}
	entries := make([]resolvedSummaryEntry, 0, len(chunks))
	for _, chunk := range chunks {
		entry, err := m.resolveChunkEntry(ctx, sessionID, chunk, counter)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return coalesceUnavailableEntries(entries, counter), nil
}

func coalesceUnavailableEntries(entries []resolvedSummaryEntry, counter TokenCounter) []resolvedSummaryEntry {
	if len(entries) <= 1 {
		return entries
	}
	out := make([]resolvedSummaryEntry, 0, len(entries))
	for i := range entries {
		cur := entries[i]
		if cur.Degraded && len(out) > 0 && out[len(out)-1].Degraded {
			prev := &out[len(out)-1]
			*prev = unavailableEntry(prev.StartSeq, cur.EndSeq, prev.EventCount+cur.EventCount, counter)
			continue
		}
		out = append(out, cur)
	}
	return out
}

func (m *Module) resolveChunkEntry(ctx context.Context, sessionID string, chunk []Event, counter TokenCounter) (resolvedSummaryEntry, error) {
	startSeq := chunk[0].Sequence
	endSeq := chunk[len(chunk)-1].Sequence
	digest := sourceDigest(chunk)
	cached, found, err := lookupCachedSummary(ctx, m.store, sessionID, startSeq, endSeq, digest, m.cfg.Summary.PromptVersion, m.promptDigest, m.cfg.Summary.MaxOutputTokens)
	if err == nil && found {
		rendered := RenderSummary(cached)
		return resolvedSummaryEntry{
			ID:         cached.ID,
			Text:       rendered,
			Tokens:     max(counter.Count(rendered), 0),
			Degraded:   false,
			StartSeq:   startSeq,
			EndSeq:     endSeq,
			EventCount: len(chunk),
		}, nil
	}
	if err != nil && ctx.Err() != nil {
		return resolvedSummaryEntry{}, ctx.Err()
	}
	if m.service != nil {
		summary, serr := m.service.resolveEvents(ctx, sessionID, startSeq, endSeq, chunk)
		if serr == nil {
			rendered := RenderSummary(summary)
			return resolvedSummaryEntry{
				ID:         summary.ID,
				Text:       rendered,
				Tokens:     max(counter.Count(rendered), 0),
				Degraded:   false,
				StartSeq:   startSeq,
				EndSeq:     endSeq,
				EventCount: len(chunk),
			}, nil
		}
		if ctx.Err() != nil {
			return resolvedSummaryEntry{}, ctx.Err()
		}
		m.logger.WarnContext(ctx, "context summary degraded", "component", "context", "start_sequence", startSeq, "end_sequence", endSeq, "error", serr)
	} else {
		m.logger.WarnContext(ctx, "context summary degraded", "component", "context", "start_sequence", startSeq, "end_sequence", endSeq)
	}
	return unavailableEntry(startSeq, endSeq, len(chunk), counter), nil
}

func unavailableEntry(startSeq, endSeq uint64, eventCount int, counter TokenCounter) resolvedSummaryEntry {
	text := RenderUnavailableRange(startSeq, endSeq, eventCount)
	return resolvedSummaryEntry{
		ID:         fmt.Sprintf("ctxsum-omitted-%d-%d", startSeq, endSeq),
		Text:       text,
		Tokens:     max(counter.Count(text), 0),
		Degraded:   true,
		StartSeq:   startSeq,
		EndSeq:     endSeq,
		EventCount: eventCount,
	}
}

func groupProjectionsAndTokens(
	group *Group,
	isProtected bool,
	priorRowBySeq map[uint64]*turn.Event,
	maxBytes int,
	counter TokenCounter,
) (projections map[uint64][]ToolProjection, tokens int, err error) {
	if isProtected {
		return nil, group.Tokens, nil
	}
	texts := make([]string, 0, len(group.Events))
	for i := range group.Events {
		ev := &group.Events[i]
		if ev.Projected == nil {
			texts = append(texts, ev.Text)
			continue
		}
		row := priorRowBySeq[ev.Sequence]
		if row == nil {
			texts = append(texts, ev.Text)
			continue
		}
		partProjs, projectedText, perr := projectRowFunctionResponses(row, ev.MediaType, ev.Sequence, maxBytes)
		if perr != nil {
			return nil, 0, perr
		}
		if len(partProjs) > 0 {
			if projections == nil {
				projections = make(map[uint64][]ToolProjection)
			}
			projections[ev.Sequence] = partProjs
			texts = append(texts, projectedText)
		} else {
			texts = append(texts, ev.Text)
		}
	}
	tokens, err = sumCounts(counter, texts)
	if err != nil {
		return nil, 0, err
	}
	return projections, tokens, nil
}

func projectRowFunctionResponses(row *turn.Event, mediaType string, sequence uint64, maxBytes int) ([]ToolProjection, string, error) {
	parts := extractRowContentParts(row.Payload)
	if len(parts) == 0 {
		return nil, "", nil
	}
	responseCount := 0
	oversizedCount := 0
	for _, part := range parts {
		if !part.IsFunctionResponse {
			continue
		}
		responseCount++
		if len(part.Text) > maxBytes {
			oversizedCount++
		}
	}
	if responseCount == 0 {
		return nil, "", nil
	}
	threshold := maxBytes
	if oversizedCount == 0 {
		threshold = max(maxBytes/responseCount, 1)
	}
	var projections []ToolProjection
	var sb strings.Builder
	for _, part := range parts {
		if !part.IsFunctionResponse || len(part.Text) <= threshold {
			sb.WriteString(part.Text)
			continue
		}
		proj := projectText(part.Text, mediaType, sequence, threshold)
		projections = append(projections, ToolProjection{
			PartIndex:  part.PartIndex,
			Projection: *proj,
		})
		encoded, err := json.Marshal(map[string]any{
			"projected":    true,
			"excerpt":      proj.Excerpt,
			"sha256":       proj.Digest,
			"bytes":        proj.Bytes,
			"ref_sequence": proj.RefSequence,
		})
		if err != nil {
			return nil, "", err
		}
		sb.Write(encoded)
	}
	return projections, sb.String(), nil
}

func oldestOptionalKept(groups []Group, protected, kept map[int]bool) (int, bool) {
	bestIdx := -1
	var bestSeq uint64
	for i := range groups {
		if protected[i] || !kept[i] || len(groups[i].Events) == 0 {
			continue
		}
		seq := groups[i].Events[0].Sequence
		if bestIdx == -1 || seq < bestSeq {
			bestIdx = i
			bestSeq = seq
		}
	}
	return bestIdx, bestIdx >= 0
}

func degradeOldestSummary(parts []resolvedPart, counter TokenCounter) (int, bool) {
	changed := false
	for i := range parts {
		part := &parts[i]
		if part.Kind != PartSummary {
			continue
		}
		for j := range part.Summaries {
			if part.Summaries[j].Degraded {
				continue
			}
			entry := &part.Summaries[j]
			*entry = unavailableEntry(entry.StartSeq, entry.EndSeq, entry.EventCount, counter)
			part.Summaries = coalesceUnavailableEntries(part.Summaries, counter)
			changed = true
			break
		}
		if changed {
			break
		}
	}
	if !changed {
		return 0, false
	}
	total := 0
	for i := range parts {
		part := &parts[i]
		switch part.Kind {
		case PartVerbatim:
			total += part.GroupTokens
		case PartSummary:
			for j := range part.Summaries {
				total += part.Summaries[j].Tokens
			}
		}
	}
	return total, true
}

func collectCompactOutput(parts []resolvedPart, priorRowBySeq map[uint64]*turn.Event, usedTokens, effective int) *CompactOutput {
	items := make([]CompactItem, 0, len(parts))
	for i := range parts {
		switch parts[i].Kind {
		case PartVerbatim:
			for j := range parts[i].Group.Events {
				ev := &parts[i].Group.Events[j]
				if _, isPrior := priorRowBySeq[ev.Sequence]; !isPrior {
					continue
				}
				items = append(items, CompactItem{
					Kind:        ItemKindEvent,
					Sequence:    ev.Sequence,
					Projections: parts[i].Projections[ev.Sequence],
				})
			}
		case PartSummary:
			for j := range parts[i].Summaries {
				entry := &parts[i].Summaries[j]
				items = append(items, CompactItem{
					Kind:        ItemKindSummary,
					SummaryID:   entry.ID,
					SummaryText: entry.Text,
				})
			}
		}
	}
	return &CompactOutput{
		Items:      items,
		Compacted:  true,
		UsedTokens: usedTokens,
		Effective:  effective,
	}
}

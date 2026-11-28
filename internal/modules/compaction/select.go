package compaction

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/anggasct/aura/internal/platform/fault"
)

type SelectionMode string

const (
	SelectionNormal    SelectionMode = "normal"
	SelectionEmergency SelectionMode = "emergency"
)

type PartKind string

const (
	PartVerbatim PartKind = "verbatim"
	PartSummary  PartKind = "summary"
)

type GroupRef struct {
	Kind GroupKind
	ID   string
}

type SummaryRange struct {
	StartSequence uint64
	EndSequence   uint64
	Groups        []GroupRef
	EventCount    int
	Tokens        int
}

type SelectedPart struct {
	Kind  PartKind
	Group Group
	Range SummaryRange
}

type Selection struct {
	Mode           SelectionMode
	Parts          []SelectedPart
	UsedTokens     int
	VerbatimGroups int
	SummaryGroups  int
	SummaryEvents  int
}

type SelectionPolicy struct {
	RecentCompleteTurns int
	HighWaterRatio      float64
	MaxExcerptBytes     int
	SummaryOutputTokens int
}

func ResolveSelectionPolicy(recentCompleteTurns int, highWaterRatio float64, maxExcerptBytes int) (*SelectionPolicy, error) {
	return ResolveSelectionPolicyWithSummary(recentCompleteTurns, highWaterRatio, maxExcerptBytes, 0)
}

func ResolveSelectionPolicyWithSummary(recentCompleteTurns int, highWaterRatio float64, maxExcerptBytes, summaryOutputTokens int) (*SelectionPolicy, error) {
	if recentCompleteTurns <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "recent complete turns must be positive")
	}
	if highWaterRatio <= 0 || highWaterRatio >= 1 {
		return nil, fault.New(ErrorCodeInvalidArgument, "high water ratio must be in (0,1)")
	}
	if maxExcerptBytes <= 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "max excerpt bytes must be positive")
	}
	if summaryOutputTokens < 0 {
		return nil, fault.New(ErrorCodeInvalidArgument, "summary output tokens must not be negative")
	}
	return &SelectionPolicy{
		RecentCompleteTurns: recentCompleteTurns,
		HighWaterRatio:      highWaterRatio,
		MaxExcerptBytes:     maxExcerptBytes,
		SummaryOutputTokens: summaryOutputTokens,
	}, nil
}

func Select(policy *SelectionPolicy, plan *Plan, counter TokenCounter) (*Selection, error) {
	if policy == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "policy must not be nil")
	}
	if plan == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "plan must not be nil")
	}
	if counter == nil {
		counter = ConservativeEstimator{}
	}
	trigger := int(float64(plan.Budget.Usable) * policy.HighWaterRatio)
	groups := copyGroups(plan.Groups)
	if plan.TotalEventTokens <= trigger && plan.TotalEventTokens <= plan.Budget.Effective {
		return levelSelection(groups, nil, plan.TotalEventTokens, SelectionNormal), nil
	}
	protected := markProtected(groups, policy.RecentCompleteTurns)
	projected, err := projectOversizedToolResults(groups, protected, policy.MaxExcerptBytes, counter)
	if err != nil {
		return nil, err
	}
	protectedTokens := 0
	for index := range groups {
		if !protected[index] {
			continue
		}
		protectedTokens, err = checkedAdd(protectedTokens, groups[index].Tokens)
		if err != nil {
			return nil, err
		}
	}
	if protectedTokens > plan.Budget.Effective {
		return nil, fault.New(ErrorCodeBudgetExceeded, "protected content exceeds the validated budget")
	}
	remaining := plan.Budget.Effective - protectedTokens
	included := make(map[int]bool, len(groups))
	includedOrder := make([]int, 0, len(groups))
	includedTokens := 0
	for _, index := range newestFirst(groups, protected) {
		if groups[index].Tokens > remaining {
			continue
		}
		included[index] = true
		includedOrder = append(includedOrder, index)
		remaining -= groups[index].Tokens
		includedTokens, err = checkedAdd(includedTokens, groups[index].Tokens)
		if err != nil {
			return nil, err
		}
	}
	if policy.SummaryOutputTokens > 0 {
		for len(includedOrder) > 0 {
			ranges := countOmittedRanges(len(groups), protected, included)
			if ranges == 0 {
				break
			}
			reserve, err := multiplyTokens(ranges, policy.SummaryOutputTokens)
			if err != nil {
				return nil, err
			}
			total, err := checkedAdd(protectedTokens, includedTokens)
			if err != nil {
				return nil, err
			}
			total, err = checkedAdd(total, reserve)
			if err != nil {
				return nil, err
			}
			if total <= plan.Budget.Effective {
				break
			}
			oldest := includedOrder[len(includedOrder)-1]
			includedOrder = includedOrder[:len(includedOrder)-1]
			delete(included, oldest)
			includedTokens -= groups[oldest].Tokens
		}
	}
	var omitted []int
	for i := range groups {
		if !protected[i] && !included[i] {
			omitted = append(omitted, i)
		}
	}
	if len(omitted) == 0 && projected == 0 {
		return levelSelection(copyGroups(plan.Groups), nil, plan.TotalEventTokens, SelectionNormal), nil
	}
	used, err := checkedAdd(protectedTokens, includedTokens)
	if err != nil {
		return nil, err
	}
	if len(omitted) > 0 && policy.SummaryOutputTokens > 0 {
		ranges := countOmittedRanges(len(groups), protected, included)
		reserve, err := multiplyTokens(ranges, policy.SummaryOutputTokens)
		if err != nil {
			return nil, err
		}
		avail := plan.Budget.Effective - used
		used, err = checkedAdd(used, min(reserve, max(avail, 0)))
		if err != nil {
			return nil, err
		}
	}
	return levelSelection(groups, omitted, used, SelectionEmergency), nil
}

func countOmittedRanges(total int, protected, included map[int]bool) int {
	ranges := 0
	inOmitted := false
	for i := range total {
		omitted := !protected[i] && !included[i]
		if omitted && !inOmitted {
			ranges++
		}
		inOmitted = omitted
	}
	return ranges
}

func multiplyTokens(count, perRange int) (int, error) {
	total := 0
	for range count {
		next, err := checkedAdd(total, perRange)
		if err != nil {
			return 0, err
		}
		total = next
	}
	return total, nil
}

func copyGroups(groups []Group) []Group {
	out := make([]Group, 0, len(groups))
	for i := range groups {
		dup := groups[i]
		dup.Events = append([]Event(nil), groups[i].Events...)
		out = append(out, dup)
	}
	return out
}

func projectOversizedToolResults(groups []Group, protected map[int]bool, maxBytes int, counter TokenCounter) (int, error) {
	projected := 0
	for i := range groups {
		if protected[i] {
			continue
		}
		group := &groups[i]
		texts := make([]string, 0, len(group.Events))
		for j := range group.Events {
			event := &group.Events[j]
			if event.Projected == nil && event.ToolResult && len(event.Text) > maxBytes {
				event.Projected = projectText(event.Text, event.MediaType, event.Sequence, maxBytes)
				event.Text = event.Projected.Excerpt
				projected++
			}
			texts = append(texts, event.Text)
		}
		tokens, err := sumCounts(counter, texts)
		if err != nil {
			return 0, err
		}
		group.Tokens = tokens
	}
	return projected, nil
}

func projectText(text, mediaType string, sequence uint64, maxBytes int) *Projection {
	head := text
	if len(head) > maxBytes {
		head = head[:maxBytes]
		for head != "" && !utf8.ValidString(head) {
			head = head[:len(head)-1]
		}
	}
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:])
	excerpt := head
	if omitted := len(text) - len(head); omitted > 0 {
		excerpt += fmt.Sprintf("\n[omitted %d of %d bytes, sha256:%s]", omitted, len(text), digest[:16])
	}
	return &Projection{
		Digest:      digest,
		Bytes:       len(text),
		Excerpt:     excerpt,
		RefSequence: sequence,
		MediaType:   mediaType,
	}
}

func markProtected(groups []Group, recentTurns int) map[int]bool {
	protected := make(map[int]bool, len(groups))
	for i := range groups {
		if groups[i].Protected {
			protected[i] = true
		}
	}
	turns := make([]int, 0, len(groups))
	for i := range groups {
		if groups[i].Kind == GroupTurn && !groups[i].Protected && len(groups[i].Events) > 0 {
			turns = append(turns, i)
		}
	}
	slices.SortFunc(turns, func(a, b int) int {
		return cmp.Compare(groups[b].Events[0].Sequence, groups[a].Events[0].Sequence)
	})
	window := uint64(0)
	for n, index := range turns {
		if n >= recentTurns {
			break
		}
		protected[index] = true
		if start := groups[index].Events[0].Sequence; window == 0 || start < window {
			window = start
		}
	}
	if window > 0 {
		for i := range groups {
			if protected[i] || len(groups[i].Events) == 0 {
				continue
			}
			if groups[i].Events[len(groups[i].Events)-1].Sequence >= window {
				protected[i] = true
			}
		}
	}
	return protected
}

func newestFirst(groups []Group, protected map[int]bool) []int {
	order := make([]int, 0, len(groups))
	for i := range groups {
		if !protected[i] && len(groups[i].Events) > 0 {
			order = append(order, i)
		}
	}
	slices.SortFunc(order, func(a, b int) int {
		return cmp.Compare(groups[b].Events[0].Sequence, groups[a].Events[0].Sequence)
	})
	return order
}

func levelSelection(groups []Group, omitted []int, used int, mode SelectionMode) *Selection {
	omittedSet := make(map[int]bool, len(omitted))
	for _, index := range omitted {
		omittedSet[index] = true
	}
	selection := &Selection{Mode: mode, UsedTokens: used}
	for i := range groups {
		if !omittedSet[i] {
			selection.Parts = append(selection.Parts, SelectedPart{Kind: PartVerbatim, Group: groups[i]})
			selection.VerbatimGroups++
			continue
		}
		if i > 0 && omittedSet[i-1] {
			continue
		}
		end := i
		for end+1 < len(groups) && omittedSet[end+1] {
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
		selection.Parts = append(selection.Parts, SelectedPart{Kind: PartSummary, Range: span})
		selection.SummaryGroups += len(span.Groups)
		selection.SummaryEvents += span.EventCount
	}
	return selection
}

package context

import (
	"reflect"
	"strings"
	"testing"
)

func selectPlanner() *Planner {
	planner, err := NewPlanner(100, 0.80, exactCounter{})
	if err != nil {
		panic(err)
	}
	return planner
}

func selectPolicy() *SelectionPolicy {
	policy, err := ResolveSelectionPolicy(2, 0.90, 64)
	if err != nil {
		panic(err)
	}
	return policy
}

func historyTurn(sequence uint64, turn, text string) Event {
	return Event{Sequence: sequence, TurnID: turn, InvocationID: "inv-old", Kind: "message.completed", Text: text}
}

func toolResultEvent(sequence uint64, turn, text string) Event {
	event := historyTurn(sequence, turn, text)
	event.Kind = "tool.completed"
	event.ToolResult = true
	event.MediaType = "text/plain"
	return event
}

func selectPlan(t *testing.T, window int, events []Event, invocation Invocation) *Plan {
	t.Helper()
	plan, err := selectPlanner().Plan(Capability{ID: "m", ContextTokens: window}, invocation, events)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return plan
}

func TestSelectNormalKeepsEverythingVerbatim(t *testing.T) {
	events := []Event{
		historyTurn(1, "t1", "first"),
		historyTurn(2, "t2", "second"),
		{Sequence: 3, TurnID: "t3", InvocationID: "inv-new", Kind: "model.delta", Text: "live"},
	}
	plan := selectPlan(t, 100000, events, Invocation{InvocationID: "inv-new"})
	selection, err := Select(selectPolicy(), plan, exactCounter{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.Mode != SelectionNormal {
		t.Errorf("mode = %q", selection.Mode)
	}
	if len(selection.Parts) != 3 || selection.SummaryGroups != 0 {
		t.Errorf("selection = %+v", selection)
	}
	for _, part := range selection.Parts {
		if part.Kind != PartVerbatim {
			t.Errorf("part = %+v", part)
		}
	}
	if selection.UsedTokens != plan.HeadTokens+plan.TotalEventTokens {
		t.Errorf("used = %d", selection.UsedTokens)
	}
}

func TestSelectEmergencySummarizesOldest(t *testing.T) {
	var events []Event
	for i := range uint64(8) {
		events = append(events, historyTurn(i+1, "t"+string(rune('a'+i)), strings.Repeat("x", 120)))
	}
	events = append(events, Event{Sequence: 9, TurnID: "t-live", InvocationID: "inv-new", Kind: "model.delta", Text: "live"})
	plan := selectPlan(t, 1000, events, Invocation{MaxOutputTokens: 50, InvocationID: "inv-new"})
	selection, err := Select(selectPolicy(), plan, exactCounter{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.Mode != SelectionEmergency {
		t.Fatalf("mode = %q", selection.Mode)
	}
	if selection.UsedTokens > plan.Budget.Effective {
		t.Errorf("used %d exceeds effective %d", selection.UsedTokens, plan.Budget.Effective)
	}
	if selection.SummaryGroups == 0 || selection.VerbatimGroups == 0 {
		t.Errorf("selection = %+v", selection)
	}
	var sawCurrent, sawRecent bool
	for _, part := range selection.Parts {
		if part.Kind != PartVerbatim {
			continue
		}
		switch part.Group.ID {
		case "inv-new":
			sawCurrent = true
		case "tg", "th":
			sawRecent = true
		}
	}
	if !sawCurrent || !sawRecent {
		t.Errorf("protected groups not verbatim: %+v", selection.Parts)
	}
	for _, part := range selection.Parts {
		if part.Kind != PartSummary {
			continue
		}
		if part.Range.StartSequence >= part.Range.EndSequence && part.Range.EventCount > 1 {
			t.Errorf("range not ordered: %+v", part.Range)
		}
	}
}

func TestSelectProjectsPriorOversizedToolResults(t *testing.T) {
	big := strings.Repeat("v", 500)
	events := []Event{
		toolResultEvent(1, "t1", big),
		historyTurn(2, "t2", "middle"),
		historyTurn(3, "t3", "latest"),
		{Sequence: 4, TurnID: "t4", InvocationID: "inv-new", Kind: "tool.completed", ToolResult: true, Text: big},
	}
	plan := selectPlan(t, 1200, events, Invocation{InvocationID: "inv-new"})
	selection, err := Select(selectPolicy(), plan, exactCounter{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	byID := make(map[string]SelectedPart)
	for _, part := range selection.Parts {
		if part.Kind == PartVerbatim {
			byID[part.Group.ID] = part
		}
	}
	old, ok := byID["t1"]
	if !ok || len(old.Group.Events) != 1 {
		t.Fatalf("parts = %+v", selection.Parts)
	}
	projected := old.Group.Events[0].Projected
	if projected == nil {
		t.Fatalf("prior oversized tool result kept verbatim")
	}
	if projected.Bytes != 500 || projected.RefSequence != 1 || projected.MediaType != "text/plain" {
		t.Errorf("projection = %+v", projected)
	}
	if len(projected.Digest) != 64 || !strings.Contains(old.Group.Events[0].Text, "omitted") {
		t.Errorf("projection = %+v", projected)
	}
	live, ok := byID["inv-new"]
	if !ok || live.Group.Events[0].Projected != nil {
		t.Errorf("current-invocation result must stay verbatim: %+v", live)
	}
}

func TestSelectKeepsRecentTurnToolResultsVerbatim(t *testing.T) {
	big := strings.Repeat("w", 300)
	events := []Event{
		historyTurn(1, "t1", "old"),
		toolResultEvent(2, "t2", big),
		toolResultEvent(3, "t3", big),
	}
	plan := selectPlan(t, 800, events, Invocation{InvocationID: "inv-new"})
	selection, err := Select(selectPolicy(), plan, exactCounter{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.Mode != SelectionEmergency {
		t.Fatalf("mode = %q, want pressure", selection.Mode)
	}
	for _, part := range selection.Parts {
		if part.Kind != PartVerbatim {
			continue
		}
		for _, event := range part.Group.Events {
			if event.ToolResult && event.Projected != nil {
				t.Errorf("recent result projected without pressure: %+v", event)
			}
		}
	}
}

func TestSelectProtectsLinkedGroupsInRecentWindow(t *testing.T) {
	events := []Event{
		historyTurn(1, "t1", strings.Repeat("a", 20)),
		historyTurn(2, "t2", strings.Repeat("b", 20)),
		historyTurn(3, "t3", strings.Repeat("c", 20)),
		{Sequence: 4, TurnID: "t3", InvocationID: "inv-old", Kind: "tool.requested", CorrelationID: "call-7", Text: strings.Repeat("d", 20)},
		historyTurn(5, "t4", strings.Repeat("e", 20)),
		{Sequence: 6, TurnID: "t4", InvocationID: "inv-old", Kind: "tool.completed", CorrelationID: "call-7", Text: strings.Repeat("f", 20)},
		historyTurn(7, "t5", strings.Repeat("g", 20)),
	}
	plan := selectPlan(t, 300, events, Invocation{InvocationID: "inv-new"})
	selection, err := Select(selectPolicy(), plan, exactCounter{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.Mode != SelectionEmergency {
		t.Fatalf("mode = %q, want pressure", selection.Mode)
	}
	for _, part := range selection.Parts {
		if part.Kind == PartSummary {
			for _, ref := range part.Range.Groups {
				if ref.ID == "call-7" {
					t.Errorf("linked group in recent window summarized: %+v", part.Range)
				}
			}
		}
	}
}

func TestSelectBudgetExceededKeepsProtectedWhole(t *testing.T) {
	events := []Event{
		historyTurn(1, "t1", strings.Repeat("x", 400)),
		{Sequence: 2, TurnID: "t2", InvocationID: "inv-new", Kind: "model.delta", Text: strings.Repeat("y", 400)},
	}
	plan, err := selectPlanner().Plan(Capability{ID: "m", ContextTokens: 950}, Invocation{InvocationID: "inv-new"}, events)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := Select(selectPolicy(), plan, exactCounter{}); err == nil {
		t.Fatalf("expected budget error")
	} else if code, ok := CodeOf(err); !ok || code != ErrorCodeBudgetExceeded {
		t.Fatalf("code = %v, %v (%v)", code, ok, err)
	}
}

func TestSelectHighWaterTriggersEmergencyBeforeFull(t *testing.T) {
	events := []Event{
		toolResultEvent(1, "t1", strings.Repeat("x", 920)),
		historyTurn(2, "t2", "recent-a"),
		historyTurn(3, "t3", "recent-b"),
	}
	plan, err := selectPlanner().Plan(Capability{ID: "m", ContextTokens: 1200}, Invocation{}, events)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	policy, err := ResolveSelectionPolicy(2, 0.90, 64)
	if err != nil {
		t.Fatalf("ResolveSelectionPolicy: %v", err)
	}
	selection, err := Select(policy, plan, exactCounter{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if selection.Mode != SelectionEmergency {
		t.Errorf("mode = %q, want emergency past high water", selection.Mode)
	}
	if selection.SummaryGroups != 0 {
		t.Errorf("nothing needed summarizing: %+v", selection)
	}
	found := false
	for _, part := range selection.Parts {
		if part.Kind == PartVerbatim && part.Group.ID == "t1" {
			found = true
			if part.Group.Events[0].Projected == nil {
				t.Errorf("emergency left the oversized result whole")
			}
		}
	}
	if !found {
		t.Errorf("t1 missing from selection: %+v", selection.Parts)
	}
	if selection.UsedTokens > plan.Budget.Effective {
		t.Errorf("used %d exceeds effective %d", selection.UsedTokens, plan.Budget.Effective)
	}
}

func TestSelectionNeverExceedsValidatedBudget(t *testing.T) {
	for _, window := range []int{500, 1000, 5000} {
		var events []Event
		sequence := uint64(0)
		next := func() uint64 {
			sequence++
			return sequence
		}
		for range 12 {
			events = append(events,
				historyTurn(next(), "t1", strings.Repeat("z", 30)),
				toolResultEvent(next(), "t1", strings.Repeat("q", 200)),
			)
		}
		plan, err := selectPlanner().Plan(Capability{ID: "m", ContextTokens: window}, Invocation{InvocationID: "inv-new"}, events)
		if err != nil {
			if code, _ := CodeOf(err); code == ErrorCodeBudgetExceeded {
				continue
			}
			t.Fatalf("Plan: %v", err)
		}
		selection, err := Select(selectPolicy(), plan, exactCounter{})
		if err != nil {
			if code, _ := CodeOf(err); code == ErrorCodeBudgetExceeded {
				continue
			}
			t.Fatalf("Select: %v", err)
		}
		if selection.UsedTokens > plan.Budget.Effective {
			t.Errorf("window %d: used %d exceeds effective %d", window, selection.UsedTokens, plan.Budget.Effective)
		}
	}
}

func TestSelectLeavesInputsUntouched(t *testing.T) {
	events := []Event{
		toolResultEvent(1, "t1", strings.Repeat("v", 500)),
		historyTurn(2, "t2", "mid"),
		historyTurn(3, "t3", "new"),
		{Sequence: 4, TurnID: "t4", InvocationID: "inv-new", Kind: "model.delta", Text: "live"},
	}
	plan := selectPlan(t, 700, events, Invocation{InvocationID: "inv-new"})
	before := copyGroups(plan.Groups)
	first, err := Select(selectPolicy(), plan, exactCounter{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !reflect.DeepEqual(before, plan.Groups) {
		t.Fatalf("selection mutated its input")
	}
	second, err := Select(selectPolicy(), plan, exactCounter{})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("selection is not deterministic")
	}
}

func TestResolveSelectionPolicyRejects(t *testing.T) {
	for name, args := range map[string]struct {
		turns   int
		ratio   float64
		excerpt int
	}{
		"zero turns":   {turns: 0, ratio: 0.9, excerpt: 64},
		"zero ratio":   {turns: 2, ratio: 0.0, excerpt: 64},
		"unit ratio":   {turns: 2, ratio: 1.0, excerpt: 64},
		"zero excerpt": {turns: 2, ratio: 0.9, excerpt: 0},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveSelectionPolicy(args.turns, args.ratio, args.excerpt); err == nil {
				t.Errorf("expected error")
			} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Errorf("code = %v, %v (%v)", code, ok, err)
			}
		})
	}
}

func TestSelectRejectsNil(t *testing.T) {
	plan := selectPlan(t, 100000, []Event{historyTurn(1, "t1", "x")}, Invocation{})
	if _, err := Select(nil, plan, exactCounter{}); err == nil {
		t.Errorf("expected error for nil policy")
	}
	if _, err := Select(selectPolicy(), nil, exactCounter{}); err == nil {
		t.Errorf("expected error for nil plan")
	}
}

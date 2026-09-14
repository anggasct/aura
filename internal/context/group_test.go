package context

import (
	"testing"
)

func turnEvent(sequence uint64, turn, text string) Event {
	return Event{Sequence: sequence, TurnID: turn, InvocationID: "inv-old", Kind: "message.completed", Text: text}
}

func TestGroupsByTurn(t *testing.T) {
	events := []Event{
		turnEvent(1, "t1", "hello"),
		turnEvent(2, "t1", "world"),
		turnEvent(3, "t2", "next"),
	}
	groups, err := GroupEvents(events, exactCounter{}, "inv-new")
	if err != nil {
		t.Fatalf("GroupEvents: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].Kind != GroupTurn || groups[0].ID != "t1" || len(groups[0].Events) != 2 {
		t.Errorf("first = %+v", groups[0])
	}
	if groups[0].Events[0].Sequence != 1 || groups[0].Events[1].Sequence != 2 {
		t.Errorf("order = %+v", groups[0].Events)
	}
	if groups[1].ID != "t2" || len(groups[1].Events) != 1 {
		t.Errorf("second = %+v", groups[1])
	}
}

func TestLinkedEventsSpanTurns(t *testing.T) {
	events := []Event{
		{Sequence: 1, TurnID: "t1", InvocationID: "inv-old", Kind: "tool.requested", CorrelationID: "call-9", Text: "run tests"},
		turnEvent(2, "t1", "filler"),
		{Sequence: 3, TurnID: "t2", InvocationID: "inv-old", Kind: "tool.completed", CorrelationID: "call-9", Text: "pass"},
	}
	groups, err := GroupEvents(events, exactCounter{}, "inv-new")
	if err != nil {
		t.Fatalf("GroupEvents: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].Kind != GroupLinked || groups[0].ID != "call-9" || len(groups[0].Events) != 2 {
		t.Errorf("linked = %+v", groups[0])
	}
	if groups[0].Events[0].Sequence != 1 || groups[0].Events[1].Sequence != 3 {
		t.Errorf("linked order = %+v", groups[0].Events)
	}
}

func TestCurrentInvocationFormsOneProtectedGroup(t *testing.T) {
	events := []Event{
		turnEvent(1, "t1", "old work"),
		{Sequence: 2, TurnID: "t2", InvocationID: "inv-new", Kind: "model.delta", CorrelationID: "call-1", Text: "draft"},
		{Sequence: 3, TurnID: "t3", InvocationID: "inv-new", Kind: "tool.requested", CorrelationID: "call-1", Text: "tool"},
	}
	groups, err := GroupEvents(events, exactCounter{}, "inv-new")
	if err != nil {
		t.Fatalf("GroupEvents: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	current := groups[1]
	if current.Kind != GroupCurrent || current.ID != "inv-new" || len(current.Events) != 2 {
		t.Errorf("current = %+v", current)
	}
	if !current.Protected {
		t.Errorf("current group is not protected")
	}
	if groups[0].Protected {
		t.Errorf("history group must not be protected")
	}
}

func TestGroupTokensSumEventText(t *testing.T) {
	events := []Event{turnEvent(1, "t1", "hello"), turnEvent(2, "t1", "world!")}
	groups, err := GroupEvents(events, exactCounter{}, "")
	if err != nil {
		t.Fatalf("GroupEvents: %v", err)
	}
	if len(groups) != 1 || groups[0].Tokens != 11 {
		t.Errorf("groups = %+v", groups)
	}
}

func TestGroupEventsRejects(t *testing.T) {
	cases := map[string]struct {
		events  []Event
		current string
	}{
		"nil counter":    {events: []Event{turnEvent(1, "t1", "x")}},
		"zero sequence":  {events: []Event{{Sequence: 0, TurnID: "t1"}}},
		"unordered":      {events: []Event{turnEvent(2, "t1", "b"), turnEvent(1, "t1", "a")}},
		"duplicate":      {events: []Event{turnEvent(1, "t1", "a"), turnEvent(1, "t1", "b")}},
		"missing turn":   {events: []Event{{Sequence: 1}}},
		"negative count": {events: []Event{turnEvent(1, "t1", "x")}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			counter := TokenCounter(exactCounter{})
			events := tc.events
			if name == "nil counter" {
				counter = nil
			}
			if name == "negative count" {
				counter = hostileCounter{}
			}
			if _, err := GroupEvents(events, counter, tc.current); err == nil {
				t.Errorf("expected error")
			} else if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Errorf("code = %v, %v (%v)", code, ok, err)
			}
		})
	}
}

type hostileCounter struct{}

func (hostileCounter) Name() string           { return "test-hostile-v1" }
func (hostileCounter) Class() AccountingClass { return AccountingEstimated }
func (hostileCounter) Count(string) int       { return -1 }

func TestGroupEventsEmpty(t *testing.T) {
	groups, err := GroupEvents(nil, exactCounter{}, "")
	if err != nil {
		t.Fatalf("GroupEvents: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("groups = %+v", groups)
	}
}

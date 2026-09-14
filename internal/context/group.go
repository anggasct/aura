package context

import (
	"strings"
)

type GroupKind string

const (
	GroupTurn    GroupKind = "turn"
	GroupLinked  GroupKind = "linked"
	GroupCurrent GroupKind = "current"
)

type Event struct {
	ID            string
	Sequence      uint64
	TurnID        string
	InvocationID  string
	Kind          string
	CorrelationID string
	ToolResult    bool
	MediaType     string
	Text          string
	Projected     *Projection
}

type Projection struct {
	Digest      string
	Bytes       int
	Excerpt     string
	RefSequence uint64
	MediaType   string
}

type Group struct {
	Kind      GroupKind
	ID        string
	Events    []Event
	Tokens    int
	Protected bool
}

func GroupEvents(events []Event, counter TokenCounter, currentInvocationID string) ([]Group, error) {
	if counter == nil {
		return nil, errNilArgument("counter")
	}
	if len(events) == 0 {
		return nil, nil
	}
	previous := uint64(0)
	for i := range events {
		event := &events[i]
		if event.Sequence == 0 {
			return nil, Errorf(ErrorCodeInvalidArgument, "event at index %d has no sequence", i)
		}
		if i > 0 && event.Sequence <= previous {
			return nil, Errorf(ErrorCodeInvalidArgument, "events are not in ascending sequence order")
		}
		previous = event.Sequence
		if strings.TrimSpace(event.TurnID) == "" {
			return nil, Errorf(ErrorCodeInvalidArgument, "event %d carries no turn scope", event.Sequence)
		}
	}
	index := make(map[string]int)
	var groups []Group
	for i := range events {
		event := &events[i]
		kind, id := GroupTurn, event.TurnID
		switch {
		case currentInvocationID != "" && event.InvocationID == currentInvocationID:
			kind, id = GroupCurrent, event.InvocationID
		case strings.TrimSpace(event.CorrelationID) != "":
			kind, id = GroupLinked, event.CorrelationID
		}
		key := string(kind) + "\x00" + id
		at, ok := index[key]
		if !ok {
			at = len(groups)
			index[key] = at
			groups = append(groups, Group{Kind: kind, ID: id, Protected: kind == GroupCurrent})
		}
		group := &groups[at]
		group.Events = append(group.Events, *event)
	}
	ordered := make([]Group, 0, len(groups))
	for i := range groups {
		group := &groups[i]
		texts := make([]string, 0, len(group.Events))
		for j := range group.Events {
			texts = append(texts, group.Events[j].Text)
		}
		tokens, err := sumCounts(counter, texts)
		if err != nil {
			return nil, err
		}
		group.Tokens = tokens
		ordered = append(ordered, *group)
	}
	return ordered, nil
}

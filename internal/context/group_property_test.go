package context

import (
	"math/rand/v2"
	"testing"
)

func generateStream(rng *rand.Rand, size int) (stream []Event, current string) {
	current = "inv-current"
	stream = make([]Event, 0, size)
	turns := []string{"t1", "t2", "t3", "t4", "t5"}
	links := []string{"", "", "call-a", "call-b", "approval-c", "effect-d"}
	for i := range size {
		invocation := "inv-old"
		if rng.IntN(4) == 0 {
			invocation = current
		}
		stream = append(stream, Event{
			Sequence:      uint64(i + 1),
			TurnID:        turns[rng.IntN(len(turns))],
			InvocationID:  invocation,
			Kind:          "message.completed",
			CorrelationID: links[rng.IntN(len(links))],
			Text:          "payload",
		})
	}
	return stream, current
}

func groupKey(kind GroupKind, id string) string {
	return string(kind) + "\x00" + id
}

func expectedKey(event *Event, current string) string {
	switch {
	case event.InvocationID == current:
		return groupKey(GroupCurrent, current)
	case event.CorrelationID != "":
		return groupKey(GroupLinked, event.CorrelationID)
	default:
		return groupKey(GroupTurn, event.TurnID)
	}
}

func TestGroupingAtomicityProperties(t *testing.T) {
	for _, seed := range []uint64{1, 7, 42, 1337} {
		rng := rand.New(rand.NewPCG(seed, seed))
		events, current := generateStream(rng, 200)
		groups, err := GroupEvents(events, exactCounter{}, current)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		seen := make(map[uint64]int)
		for _, group := range groups {
			for i, event := range group.Events {
				seen[event.Sequence]++
				if i > 0 && event.Sequence < group.Events[i-1].Sequence {
					t.Fatalf("seed %d: group %q reordered", seed, group.ID)
				}
				if want := expectedKey(&event, current); want != groupKey(group.Kind, group.ID) {
					t.Fatalf("seed %d: event %d in wrong group", seed, event.Sequence)
				}
			}
		}
		if len(seen) != len(events) {
			t.Fatalf("seed %d: covered %d of %d events", seed, len(seen), len(events))
		}
		for sequence, count := range seen {
			if count != 1 {
				t.Fatalf("seed %d: event %d covered %d times", seed, sequence, count)
			}
		}
		previous := uint64(0)
		for _, group := range groups {
			first := group.Events[0].Sequence
			if first <= previous {
				t.Fatalf("seed %d: groups out of order", seed)
			}
			previous = first
		}
	}
}

func TestGroupingDeterministic(t *testing.T) {
	rng := rand.New(rand.NewPCG(99, 99))
	events, current := generateStream(rng, 100)
	first, err := GroupEvents(events, exactCounter{}, current)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := GroupEvents(events, exactCounter{}, current)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("group counts differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Kind != second[i].Kind || first[i].ID != second[i].ID || len(first[i].Events) != len(second[i].Events) {
			t.Fatalf("group %d differs", i)
		}
	}
}

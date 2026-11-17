package turn

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/store"
)

func TestEventBusDeliversEachEventToEverySubscriberInOrder(t *testing.T) {
	bus := NewEventBus(slog.New(slog.DiscardHandler))
	var seen []string
	for _, name := range []string{"first", "second", "third"} {
		if err := bus.Subscribe(func(ev *store.RuntimeEvent) { seen = append(seen, name+":"+ev.ID) }); err != nil {
			t.Fatalf("Subscribe(%s): %v", name, err)
		}
	}

	bus.Publish(&store.RuntimeEvent{ID: "one"})
	want := []string{"first:one", "second:one", "third:one"}
	if !slices.Equal(seen, want) {
		t.Fatalf("after the first publish seen = %v, want %v", seen, want)
	}

	bus.Publish(&store.RuntimeEvent{ID: "two"})
	want = append(want, "first:two", "second:two", "third:two")
	if !slices.Equal(seen, want) {
		t.Fatalf("after the second publish seen = %v, want %v", seen, want)
	}
}

func TestEventBusKeepsDeliveringAfterAPanickingSubscriber(t *testing.T) {
	var logs bytes.Buffer
	bus := NewEventBus(slog.New(slog.NewJSONHandler(&logs, nil)))
	if err := bus.Subscribe(func(*store.RuntimeEvent) { panic("subscriber failure") }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	var received *store.RuntimeEvent
	if err := bus.Subscribe(func(ev *store.RuntimeEvent) { received = ev }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	bus.Publish(&store.RuntimeEvent{
		ID: "evt-1", SessionID: "session-1", TurnID: "turn-1", Kind: EventKindToolRequested,
		Payload: json.RawMessage(`{"secret":"payload-marker"}`),
	})

	if received == nil || received.ID != "evt-1" {
		t.Fatalf("second subscriber received %+v, want evt-1", received)
	}
	records := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(records) != 1 {
		t.Fatalf("log records = %d, want 1: %s", len(records), logs.String())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(records[0]), &record); err != nil {
		t.Fatalf("decode log record: %v", err)
	}
	if record["level"] != "ERROR" || record["msg"] != "live event subscriber panicked" {
		t.Fatalf("log record = %v, want the error record for the panicking subscriber", record)
	}
	if record["kind"] != EventKindToolRequested || record["session_id"] != "session-1" || record["turn_id"] != "turn-1" {
		t.Fatalf("log record = %v, want the event's kind, session and turn", record)
	}
	if strings.Contains(logs.String(), "payload-marker") {
		t.Fatalf("log holds the event payload: %s", logs.String())
	}
}

func TestEventBusRefusesANilHandlerAndFallsBackFromANilLogger(t *testing.T) {
	bus := NewEventBus(nil)
	err := bus.Subscribe(nil)
	if code, ok := fault.CodeOf[ErrorCode](err); !ok || code != ErrorCodeInvalidArgument {
		t.Fatalf("Subscribe(nil) = %v, want code %q", err, ErrorCodeInvalidArgument)
	}

	var received *store.RuntimeEvent
	if err := bus.Subscribe(func(ev *store.RuntimeEvent) { received = ev }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	bus.Publish(&store.RuntimeEvent{ID: "evt-1"})
	if received == nil || received.ID != "evt-1" {
		t.Fatalf("subscriber received %+v, want evt-1", received)
	}
	bus.Publish(nil)
}

func TestEventBusPublishesWhileHandlersSubscribe(t *testing.T) {
	const workers = 8
	const events = 50
	bus := NewEventBus(slog.New(slog.DiscardHandler))
	var seen [workers]sync.Map
	var ready [workers]chan struct{}
	for k := range ready {
		ready[k] = make(chan struct{})
	}

	var wg sync.WaitGroup
	for k := range workers {
		wg.Go(func() {
			if err := bus.Subscribe(func(ev *store.RuntimeEvent) { seen[k].Store(ev.ID, true) }); err != nil {
				t.Errorf("Subscribe: %v", err)
			}
			close(ready[k])
		})
	}
	for p := range workers {
		wg.Go(func() {
			for n := range events {
				id := fmt.Sprintf("publisher-%d-event-%d", p, n)
				var subscribed []int
				for k := range workers {
					select {
					case <-ready[k]:
						subscribed = append(subscribed, k)
					default:
					}
				}
				bus.Publish(&store.RuntimeEvent{ID: id})
				for _, k := range subscribed {
					if _, ok := seen[k].Load(id); !ok {
						t.Errorf("handler %d subscribed before %s was published and did not receive it", k, id)
					}
				}
			}
		})
	}
	wg.Wait()
}

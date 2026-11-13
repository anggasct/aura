package toolbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/tools"
)

type gatedCalls struct {
	timeline *callTimeline
	release  map[string]chan struct{}
	open     map[string]func()
}

func newGatedCalls(names ...string) *gatedCalls {
	gated := &gatedCalls{timeline: &callTimeline{}, release: map[string]chan struct{}{}, open: map[string]func(){}}
	for _, name := range names {
		gate := make(chan struct{})
		gated.release[name] = gate
		gated.open[name] = sync.OnceFunc(func() { close(gate) })
	}
	return gated
}

func (g *gatedCalls) openAll() {
	for _, open := range g.open {
		open()
	}
}

func (g *gatedCalls) adapter(name string) Adapter {
	return func(ctx context.Context, _ *ToolRequest, _ approval.Constraints) (ToolResult, error) {
		g.timeline.add("start:" + name)
		select {
		case <-g.release[name]:
		case <-ctx.Done():
			return ToolResult{}, ctx.Err()
		}
		g.timeline.add("end:" + name)
		return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	}
}

func ownerCall(toolName string, i int) *ToolRequest {
	request := brokerRequest(toolName, fmt.Sprintf(`{"call":%d}`, i))
	request.RequestID = fmt.Sprintf("request-%d", i)
	request.IdempotencyKey = fmt.Sprintf("idempotency-%d", i)
	request.AllocateEventSequence = true
	return request
}

func TestToolCallThatChangesStateRunsAlone(t *testing.T) {
	cases := []struct {
		name  string
		alone tools.Definition
	}{
		{name: "an effectful call", alone: tools.Definition{Name: "alone", Effectful: true}},
		{name: "an orchestration call", alone: tools.Definition{Name: "alone", Orchestration: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			effects := newBrokerEffectExecutor(t)
			synctest.Test(t, func(t *testing.T) {
				gated := newGatedCalls("read-before", "alone", "read-after")
				defer gated.openAll()
				rig := newBatchRig(t, 3, &Options{Effects: effects})
				for _, name := range []string{"read-before", "alone", "read-after"} {
					definition := &tools.Definition{Name: name}
					if name == "alone" {
						definition = &tc.alone
					}
					rig.register(t, definition, gated.adapter(name), &approval.Rule{})
				}

				outcomes := []<-chan callOutcome{
					rig.start(t, 0, ownerCall("read-before", 0)),
					rig.start(t, 1, ownerCall("alone", 1)),
					rig.start(t, 2, ownerCall("read-after", 2)),
				}

				requireTimeline(t, gated.timeline, "start:read-before")
				gated.open["read-before"]()
				requireTimeline(t, gated.timeline, "start:read-before", "end:read-before", "start:alone")
				gated.open["alone"]()
				requireTimeline(t, gated.timeline, "start:read-before", "end:read-before", "start:alone", "end:alone", "start:read-after")
				gated.open["read-after"]()
				for i, outcome := range outcomes {
					requireAnswered(t, fmt.Sprintf("call %d", i), outcome)
				}
			})
		})
	}
	t.Run("calls that do not change state overlap", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			gated := newGatedCalls("read-before", "read-after")
			defer gated.openAll()
			rig := newBatchRig(t, 2, &Options{})
			for _, name := range []string{"read-before", "read-after"} {
				rig.register(t, &tools.Definition{Name: name}, gated.adapter(name), &approval.Rule{})
			}

			outcomes := []<-chan callOutcome{
				rig.start(t, 0, ownerCall("read-before", 0)),
				rig.start(t, 1, ownerCall("read-after", 1)),
			}

			synctest.Wait()
			if got := slices.Sorted(slices.Values(gated.timeline.list())); !slices.Equal(got, []string{"start:read-after", "start:read-before"}) {
				t.Fatalf("timeline = %q, want both reads started before either ended", got)
			}
			gated.open["read-before"]()
			gated.open["read-after"]()
			for i, outcome := range outcomes {
				requireAnswered(t, fmt.Sprintf("call %d", i), outcome)
			}
		})
	})
}

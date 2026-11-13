package toolbroker

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolcall"
	"github.com/anggasct/aura/internal/platform/tools"
)

type callTimeline struct {
	mu     sync.Mutex
	events []string
}

func (c *callTimeline) add(event string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *callTimeline) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.events)
}

func (c *callTimeline) indexOf(event string) int {
	return slices.Index(c.list(), event)
}

func requireTimeline(t *testing.T, timeline *callTimeline, want ...string) {
	t.Helper()
	synctest.Wait()
	if got := timeline.list(); !slices.Equal(got, want) {
		t.Fatalf("timeline = %q, want %q", got, want)
	}
}

type orderedAccess struct {
	mu       sync.Mutex
	ceiling  []string
	standing map[string]bool
}

func (a *orderedAccess) ToolCeiling(agentID string, class principal.Class) ([]string, bool) {
	if agentID != "team" || class != principal.ClassTrusted {
		return nil, false
	}
	return slices.Clone(a.ceiling), true
}

func (a *orderedAccess) StandingGrant(_ context.Context, _, _, toolName, _ string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.standing[toolName], nil
}

func (a *orderedAccess) grant(toolName string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.standing[toolName] = true
}

type batchRig struct {
	broker *Broker
	batch  *toolcall.Batch
	scope  *durable.TurnScope
}

func newBatchRig(t *testing.T, size int, options *Options) *batchRig {
	t.Helper()
	options.HostCapabilities = capabilitytest.Host(t)
	broker, err := New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fake := durable.NewFake()
	invocations := make(chan durable.Invocation, 1)
	fake.RegisterHandler("batch", func(ctx context.Context, inv durable.Invocation) error {
		invocations <- inv
		<-ctx.Done()
		return nil
	})
	if _, err := fake.Start(t.Context(), durable.StartRequest{Handler: "batch", Key: "turn-1"}); err != nil {
		t.Fatalf("start the fake run: %v", err)
	}
	t.Cleanup(fake.Close)
	inv := <-invocations
	return &batchRig{broker: broker, batch: toolcall.NewBatch(1, size), scope: durable.NewTurnScope(inv)}
}

func (r *batchRig) register(t *testing.T, definition *tools.Definition, adapter Adapter, rule *approval.Rule) {
	t.Helper()
	definition.Version = "v1"
	definition.Validator = func(raw json.RawMessage) (json.RawMessage, error) { return raw, nil }
	rule.ToolName, rule.ToolVersion = definition.Name, "v1"
	if err := r.broker.RegisterTool(definition, adapter, rule); err != nil {
		t.Fatalf("register %s: %v", definition.Name, err)
	}
}

func (r *batchRig) start(t *testing.T, i int, request *ToolRequest) <-chan callOutcome {
	t.Helper()
	slot := r.batch.Slot(i)
	ctx := toolcall.With(durable.WithTurnScope(t.Context(), r.scope), slot)
	out := make(chan callOutcome, 1)
	go func() {
		defer slot.Close()
		result, err := r.broker.Execute(ctx, request)
		out <- outcomeOf(result, err)
	}()
	return out
}

func requireAnswered(t *testing.T, what string, out <-chan callOutcome) {
	t.Helper()
	synctest.Wait()
	select {
	case got := <-out:
		if got != probeSucceeded {
			t.Errorf("%s = %+v, want %+v", what, got, probeSucceeded)
		}
	default:
		t.Errorf("%s is still running", what)
	}
}

func trustedCall(toolName string, i int) *ToolRequest {
	request := classifiedRequest(toolName, fmt.Sprintf(`{"call":%d}`, i), principal.ClassTrusted)
	request.RequestID = fmt.Sprintf("request-%d", i)
	request.IdempotencyKey = fmt.Sprintf("idempotency-%d", i)
	return request
}

func TestToolCallsAreAuthorizedInCallOrder(t *testing.T) {
	cases := []struct {
		name      string
		standing  bool
		wantAsked []string
	}{
		{name: "each question waits for the earlier answer", wantAsked: []string{"ask-first", "ask-last"}},
		{name: "a standing grant on an earlier call covers a later call", standing: true, wantAsked: []string{"ask-first"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				timeline := &callTimeline{}
				access := &orderedAccess{ceiling: []string{"ask-first", "open", "ask-last"}, standing: map[string]bool{}}
				answers := map[string]chan struct{}{"ask-first": make(chan struct{}), "ask-last": make(chan struct{})}
				answer := map[string]func(){}
				for name, gate := range answers {
					answer[name] = sync.OnceFunc(func() { close(gate) })
				}
				defer func() {
					for _, open := range answer {
						open()
					}
				}()
				rig := newBatchRig(t, 3, &Options{
					Access: access,
					ApprovalDecider: func(_ context.Context, prompt *ApprovalPrompt) (bool, error) {
						timeline.add("asked:" + prompt.ToolName)
						<-answers[prompt.ToolName]
						if tc.standing && prompt.ToolName == "ask-first" {
							access.grant("ask-last")
						}
						timeline.add("answered:" + prompt.ToolName)
						return true, nil
					},
				})
				for _, name := range []string{"ask-first", "open", "ask-last"} {
					rig.register(t, &tools.Definition{Name: name}, func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
						timeline.add("ran:" + name)
						return ToolResult{Output: json.RawMessage(`{"ok":true}`)}, nil
					}, &approval.Rule{RequiresApproval: name != "open"})
				}

				first := rig.start(t, 0, trustedCall("ask-first", 0))
				second := rig.start(t, 1, trustedCall("open", 1))
				third := rig.start(t, 2, trustedCall("ask-last", 2))

				requireTimeline(t, timeline, "asked:ask-first")
				answer["ask-first"]()
				synctest.Wait()
				if tc.standing {
					events := timeline.list()
					if len(events) != 5 || !slices.Equal(events[:2], []string{"asked:ask-first", "answered:ask-first"}) {
						t.Fatalf("timeline = %q, want the first question and answer, then the three executions", events)
					}
					if rest := slices.Sorted(slices.Values(events[2:])); !slices.Equal(rest, []string{"ran:ask-first", "ran:ask-last", "ran:open"}) {
						t.Errorf("executions after the first answer = %q, want all three calls", rest)
					}
				} else {
					events := timeline.list()
					if !slices.Contains(events, "asked:ask-last") {
						t.Fatalf("timeline = %q, want the question of the last call after the first answer", events)
					}
					if timeline.indexOf("answered:ask-first") > timeline.indexOf("asked:ask-last") || timeline.indexOf("answered:ask-first") > timeline.indexOf("ran:open") {
						t.Errorf("timeline = %q, want the first answer before the later questions and executions", events)
					}
					answer["ask-last"]()
				}
				requireAnswered(t, "first call", first)
				requireAnswered(t, "second call", second)
				requireAnswered(t, "third call", third)
				var asked []string
				for _, event := range timeline.list() {
					if name, ok := strings.CutPrefix(event, "asked:"); ok {
						asked = append(asked, name)
					}
				}
				if !slices.Equal(asked, tc.wantAsked) {
					t.Errorf("questions = %q, want %q", asked, tc.wantAsked)
				}
			})
		})
	}
}

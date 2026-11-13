package toolbroker

import (
	"context"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/durable"
)

func TestScopedToolCallWithoutACallKeyIsRefused(t *testing.T) {
	hosted := startHostedRuntime(t)
	broker := newProbeBroker(t)
	probe := &probeTool{rule: probeRule("probe")}
	registerProbe(t, broker, probe)
	var outcome callOutcome
	key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
		scoped := durable.WithTurnScope(ctx, durable.NewTurnScope(inv))
		outcome = outcomeOf(broker.Execute(scoped, probeRequest("probe")))
		return nil
	})
	if outcome.class != ResultInvalidArgument || !strings.Contains(outcome.text, "has no call key on a durable turn") {
		t.Errorf("outcome = %+v, want %s without a call key", outcome, ResultInvalidArgument)
	}
	for _, row := range hosted.actionRows(t, key) {
		t.Errorf("recorded action %s, want none", row.key)
	}
	if calls := probe.calls.Load(); calls != 0 {
		t.Errorf("tool runs = %d, want 0", calls)
	}
}

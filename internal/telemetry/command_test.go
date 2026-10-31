package telemetry

import "testing"

func TestCommandRecorderNamesAndNilSafety(t *testing.T) {
	if MetricCommandInvocationsTotal != "command.invocations.total" || SpanCommandExecute != "command.execute" {
		t.Errorf("command telemetry names drifted: %s %s", MetricCommandInvocationsTotal, SpanCommandExecute)
	}
	var recorder *CommandRecorder
	ctx, finish := recorder.Start(t.Context(), "help", "terminal", "owner")
	finish("ok")
	if ctx != t.Context() {
		t.Error("nil recorder replaced the context")
	}
}

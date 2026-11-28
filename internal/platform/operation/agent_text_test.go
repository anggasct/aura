package operation_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/toolbroker"
)

func TestAgentToolFailureTextAndClass(t *testing.T) {
	tests := []struct {
		name  string
		fail  error
		class toolbroker.ResultClass
		text  string
	}{
		{"an input rejection with a detail", operation.NewError(operation.ErrorCodeInputInvalid, "subject is too long"), toolbroker.ResultInvalidArgument, "operation_input_invalid: subject is too long"},
		{"an input rejection without a detail", operation.NewError(operation.ErrorCodeInputInvalid, ""), toolbroker.ResultInvalidArgument, "operation_input_invalid"},
		{"a failure with a detail", operation.NewError(operation.ErrorCodeFailed, "the store is busy"), toolbroker.ResultExecutionFailed, "operation_failed: the store is busy"},
		{"an uncoded handler error", errors.New("open /etc/shadow"), toolbroker.ResultExecutionFailed, "operation_failed"},
		{"a cancellation", context.Canceled, toolbroker.ResultDeadlineExceeded, ""},
		{"a deadline", context.DeadlineExceeded, toolbroker.ResultDeadlineExceeded, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rig := operationtest.NewRig(t, nil)
			rig.Stubs.FailWith(operationtest.Read, tt.fail)

			outcome := rig.Execute(t.Context(), operationtest.Request(t, operationtest.ToolOf(operationtest.Read), "a"))

			if outcome.Class != tt.class || outcome.Err == nil {
				t.Fatalf("outcome = %+v, want class %s with an error", outcome, tt.class)
			}
			if tt.text == "" {
				if strings.Contains(outcome.Err.Error(), "operation_failed") {
					t.Errorf("an interrupted call reads %q, want the broker's own ended-request text", outcome.Err)
				}
				return
			}
			if !strings.HasSuffix(outcome.Err.Error(), ": "+tt.text) {
				t.Errorf("the agent reads %q, want it to end with %q", outcome.Err, tt.text)
			}
			if strings.Contains(outcome.Err.Error(), "/etc") || strings.HasSuffix(outcome.Err.Error(), ": ") {
				t.Errorf("the agent reads %q, want no handler text and no dangling separator", outcome.Err)
			}
		})
	}
}

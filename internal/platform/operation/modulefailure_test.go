package operation_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
)

const moduleCodeMissing moduleCode = "module_missing"

type otherCode string

const otherCodeBroken otherCode = "other_broken"

func TestModuleFailureRendersAModuleCodeOnce(t *testing.T) {
	cause := fault.New(moduleCodeMissing, "the thing is not there")

	got := operation.ModuleFailure[moduleCode](cause)

	if !fault.HasCode(got, operation.ErrorCodeFailed) || !fault.HasCode(got, moduleCodeMissing) {
		t.Fatalf("error = %v, want the module code kept under %s", got, operation.ErrorCodeFailed)
	}
	if !errors.Is(got, cause) {
		t.Errorf("error = %v, want the cause kept in the chain", got)
	}
	var detailed interface{ SafeDetail() string }
	if !errors.As(got, &detailed) || detailed.SafeDetail() != "the thing is not there" {
		t.Errorf("safe detail = %v, want the module's own detail", detailed)
	}
	if text := fmt.Sprint(got); strings.Count(text, string(moduleCodeMissing)) != 1 {
		t.Errorf("error %q prints the module code other than once", text)
	}
}

func TestModuleFailureLeavesOtherErrorsAlone(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "an operation error", err: operation.NewError(operation.ErrorCodeInputInvalid, "bad input")},
		{name: "a code of another package", err: fault.New(otherCodeBroken, "broken")},
		{name: "an uncoded error", err: errors.New("plain")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := operation.ModuleFailure[moduleCode](tc.err); !errors.Is(got, tc.err) {
				t.Errorf("error = %v, want %v unchanged", got, tc.err)
			}
		})
	}
	t.Run("nil", func(t *testing.T) {
		if got := operation.ModuleFailure[moduleCode](nil); got != nil {
			t.Errorf("error = %v, want nil", got)
		}
	})
}

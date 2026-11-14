package fault_test

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

type codeA string

type codeB string

const (
	a1 codeA = "invalid_argument"
	a2 codeA = "other"
	b1 codeB = "invalid_argument"
)

func TestErrorTextJoinsCodeDetailAndCause(t *testing.T) {
	cause := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"code alone", fault.New(a1, ""), "invalid_argument"},
		{"code and detail", fault.New(a1, "d"), "invalid_argument: d"},
		{"code detail cause", fault.Wrap(a1, "d", cause), "invalid_argument: d: boom"},
		{"code and cause", fault.Wrap(a1, "", cause), "invalid_argument: boom"},
		{"nil cause", fault.Wrap(a1, "d", nil), "invalid_argument: d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("text = %q, want %q", got, tc.want)
			}
		})
	}
	if errors.Unwrap(fault.Wrap(a1, "d", nil)) != nil {
		t.Fatal("a nil cause must leave Unwrap nil")
	}
}

func TestCodeOfMatchesOnlyItsCodeType(t *testing.T) {
	errA := fault.New(a1, "x")
	errB := fault.New(b1, "x")
	if c, ok := fault.CodeOf[codeA](errA); !ok || c != a1 {
		t.Fatalf("codeA on errA = %q, %v", c, ok)
	}
	if _, ok := fault.CodeOf[codeA](errB); ok {
		t.Fatal("codeA must not see a codeB error")
	}
	if _, ok := fault.CodeOf[codeB](errA); ok {
		t.Fatal("codeB must not see a codeA error")
	}
	if c, ok := fault.CodeOf[codeB](errB); !ok || c != b1 {
		t.Fatalf("codeB on errB = %q, %v", c, ok)
	}
}

func TestCodeOfReadsTheFirstErrorOfItsTypeInTheChain(t *testing.T) {
	nested := fault.Wrap(a1, "outer", fault.Wrap(a2, "inner", nil))
	if c, ok := fault.CodeOf[codeA](nested); !ok || c != a1 {
		t.Fatalf("nested = %q, %v", c, ok)
	}
	joined := fmt.Errorf("%w: %w", nested, errors.New("more"))
	if c, ok := fault.CodeOf[codeA](joined); !ok || c != a1 {
		t.Fatalf("joined = %q, %v", c, ok)
	}
}

func TestHasCodeAgreesWithCodeOf(t *testing.T) {
	nested := fault.Wrap(a1, "outer", fault.Wrap(a2, "inner", nil))
	chains := []error{nested, fmt.Errorf("%w: x", nested), fault.New(a2, ""), fault.New(b1, ""), errors.New("plain"), nil}
	for _, err := range chains {
		for _, code := range []codeA{a1, a2} {
			c, ok := fault.CodeOf[codeA](err)
			if want := ok && c == code; fault.HasCode(err, code) != want {
				t.Fatalf("HasCode(%v, %q) disagrees with CodeOf", err, code)
			}
		}
	}
	if fault.HasCode(nested, a2) {
		t.Fatal("HasCode must not report a code found deeper than the first")
	}
}

func TestWrapKeepsTheCauseForIsAndAs(t *testing.T) {
	sentinel := &fs.PathError{Op: "open", Path: "p", Err: fs.ErrNotExist}
	err := fault.Wrap(a1, "d", sentinel)
	if !errors.Is(err, sentinel) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("errors.Is must reach the cause")
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		t.Fatal("errors.As must reach the cause")
	}
	if errors.Unwrap(fault.New(a1, "d")) != nil {
		t.Fatal("New has no cause")
	}
}

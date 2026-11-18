package configkit

import (
	"errors"
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

type otherCode string

const codeOther otherCode = "other_code"

func TestFieldErrorTextAndUnder(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		cases := []struct {
			name string
			err  error
			want string
		}{
			{"path and code", Invalid("port", "must be positive"), "port: config_invalid: must be positive"},
			{"owner code", Field("name", fault.New(codeOther, "bad")), "name: other_code: bad"},
			{"position after path", &FieldError{Path: "port", Position: "line 3, column 9", Err: fault.New(ErrorCodeInvalid, "x")}, "port (line 3, column 9): config_invalid: x"},
			{"empty path", Invalid("", "x"), "config_invalid: x"},
			{"empty path with position", &FieldError{Position: "env AURA_X", Err: fault.New(ErrorCodeInvalid, "x")}, "(env AURA_X) config_invalid: x"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := tc.err.Error(); got != tc.want {
					t.Fatalf("text = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("nil stays nil", func(t *testing.T) {
		if Field("a", nil) != nil || Under("a", nil) != nil {
			t.Fatal("a nil error must stay nil")
		}
	})

	t.Run("under places a join", func(t *testing.T) {
		joined := errors.Join(Invalid("port", "must be positive"), fault.New(codeOther, "x"))
		placed := Under("server", joined)
		children, ok := placed.(interface{ Unwrap() []error })
		if !ok {
			t.Fatalf("Under returned %T, want a join", placed)
		}
		var paths []string
		for _, child := range children.Unwrap() {
			var field *FieldError
			if !errors.As(child, &field) {
				t.Fatalf("child %v is not a FieldError", child)
			}
			paths = append(paths, field.Path)
		}
		if len(paths) != 2 || paths[0] != "server.port" || paths[1] != "server" {
			t.Fatalf("paths = %v, want [server.port server]", paths)
		}
		if got, want := placed.Error(), "server.port: config_invalid: must be positive\nserver: other_code: x"; got != want {
			t.Fatalf("text = %q, want %q", got, want)
		}
	})

	t.Run("under keeps the position and the nested join", func(t *testing.T) {
		inner := errors.Join(&FieldError{Path: "a", Position: "line 2, column 3", Err: fault.New(ErrorCodeInvalid, "x")}, errors.Join(Invalid("", "y")))
		placed := Under("section", inner)
		if got, want := placed.Error(), "section.a (line 2, column 3): config_invalid: x\nsection: config_invalid: y"; got != want {
			t.Fatalf("text = %q, want %q", got, want)
		}
	})

	t.Run("codes stay reachable through under", func(t *testing.T) {
		placed := Under("server", errors.Join(Invalid("port", "p"), fault.New(codeOther, "x")))
		if !fault.HasCode(placed, ErrorCodeInvalid) {
			t.Fatal("config_invalid must stay reachable")
		}
		if !fault.HasCode(placed, codeOther) {
			t.Fatal("an owner code must stay reachable")
		}
		cause := errors.New("cause")
		wrapped := Under("server", Field("port", fault.Wrap(ErrorCodeInvalid, "d", cause)))
		if !errors.Is(wrapped, cause) {
			t.Fatal("errors.Is must reach the cause through Under")
		}
	})
}

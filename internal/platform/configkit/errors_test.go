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

func TestUnderKeepsTheKeyMarker(t *testing.T) {
	placed := Under("models", errors.Join(InvalidKey("definitions[Bad]", "has an invalid name"), Invalid("definitions[ok]", "x")))
	joined, isJoin := placed.(interface{ Unwrap() []error })
	if !isJoin {
		t.Fatalf("Under returned %T, want a join", placed)
	}
	var fields []*FieldError
	for _, child := range joined.Unwrap() {
		var field *FieldError
		if !errors.As(child, &field) {
			t.Fatalf("child %v is not a FieldError", child)
		}
		fields = append(fields, field)
	}
	if len(fields) != 2 {
		t.Fatalf("placed %d problems, want 2", len(fields))
	}
	if fields[0].Path != "models.definitions[Bad]" || !fields[0].AtKey {
		t.Fatalf("key problem = %q AtKey=%v, want models.definitions[Bad] with the marker", fields[0].Path, fields[0].AtKey)
	}
	if fields[1].Path != "models.definitions[ok]" || fields[1].AtKey {
		t.Fatalf("plain problem = %q AtKey=%v, want models.definitions[ok] without the marker", fields[1].Path, fields[1].AtKey)
	}
	if got, want := fields[0].Error(), "models.definitions[Bad]: config_invalid: has an invalid name"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestUnderPlacesIndexedPathsAfterThePrefix(t *testing.T) {
	for name, test := range map[string]struct {
		prefix, path, want string
	}{
		"an index path joins without a dot":  {"bindings", "[0].persona", "bindings[0].persona"},
		"a lone index joins without a dot":   {"bindings", "[12]", "bindings[12]"},
		"a field path joins with a dot":      {"agents", "definitions[0].name", "agents.definitions[0].name"},
		"an index inside a name keeps a dot": {"agents", "a[0]", "agents.a[0]"},
		"an empty path stays on the prefix":  {"bindings", "", "bindings"},
	} {
		t.Run(name, func(t *testing.T) {
			var field *FieldError
			if !errors.As(Under(test.prefix, Invalid(test.path, "x")), &field) {
				t.Fatal("Under must return a FieldError")
			}
			if field.Path != test.want {
				t.Fatalf("path = %q, want %q", field.Path, test.want)
			}
		})
	}
}

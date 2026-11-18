package config

import "testing"

const positionFixture = "a:\n" +
	"  b: 1\n" +
	"list:\n" +
	"  - b: 2\n" +
	"  - b: 3\n" +
	"map:\n" +
	"  key:\n" +
	"    c: x\n"

func TestPositionOfResolvesMappingSequenceAndEnv(t *testing.T) {
	t.Setenv("AURA_SERVER_PORT", "9090")
	doc, err := parseDocument([]byte(positionFixture))
	if err != nil {
		t.Fatalf("parseDocument: %v", err)
	}
	tests := []struct {
		name string
		path string
		want string
	}{
		{"mapping key", "a.b", "line 2, column 6"},
		{"mapping node", "a", "line 2, column 3"},
		{"sequence index", "list[1].b", "line 5, column 8"},
		{"first sequence index", "list[0].b", "line 4, column 8"},
		{"sequence item", "list[1]", "line 5, column 5"},
		{"bracketed map key", "map[key].c", "line 8, column 8"},
		{"plain map key", "map.key.c", "line 8, column 8"},
		{"environment", "server.port", "env AURA_SERVER_PORT"},
		{"absent path", "a.zzz", ""},
		{"absent sequence index", "list[2].b", ""},
		{"empty path", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := doc.positionOf(tt.path); got != tt.want {
				t.Errorf("positionOf(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
	t.Run("a decode failure carries the position of its value", func(t *testing.T) {
		tests := map[string]string{
			"version: 1\nserver:\n  host: 5\n":            "server.host (line 3, column 9): config_invalid: expected type 'string', got unconvertible type 'int'",
			"version: 1\nbindings:\n  - 5\n":              "bindings[0] (line 3, column 5): config_invalid: expected a map or struct, got \"int\"",
			"version: 1\nruntime:\n  turn_timeout: bad\n": "runtime.turn_timeout (line 3, column 17): config_invalid: invalid duration \"bad\": time: invalid duration \"bad\"",
		}
		for content, want := range tests {
			_, err := decodeContent(t, content)
			if err == nil || err.Error() != want {
				t.Errorf("decode error = %v, want %q", err, want)
			}
		}
	})
	t.Run("a key position points at the key", func(t *testing.T) {
		if got := doc.keyPositionOf("a.b"); got != "line 2, column 3" {
			t.Errorf("keyPositionOf(a.b) = %q, want the key position", got)
		}
		if got := doc.keyPositionOf("map.key"); got != "line 7, column 3" {
			t.Errorf("keyPositionOf(map.key) = %q, want the key position", got)
		}
	})
}

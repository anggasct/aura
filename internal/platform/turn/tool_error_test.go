package turn

import "testing"

func TestIsToolError(t *testing.T) {
	cases := []struct {
		name     string
		response map[string]any
		want     bool
	}{
		{name: "a message", response: map[string]any{"error": "boom"}, want: true},
		{name: "an empty message", response: map[string]any{"error": ""}, want: false},
		{name: "a blank message", response: map[string]any{"error": "  "}, want: false},
		{name: "a null error", response: map[string]any{"error": nil}, want: false},
		{name: "an object", response: map[string]any{"error": map[string]any{"code": "x"}}, want: true},
		{name: "a key of another case", response: map[string]any{"Error": "boom"}, want: true},
		{name: "no error key", response: map[string]any{"result": "ok"}, want: false},
		{name: "no response", response: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsToolError(tc.response); got != tc.want {
				t.Fatalf("IsToolError(%v) = %v, want %v", tc.response, got, tc.want)
			}
		})
	}
}

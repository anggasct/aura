package workspace

import (
	"testing"

	"github.com/anggasct/aura/internal/platform/fault"
)

func TestValidateRelative(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "a file", path: "docs/policy.md"},
		{name: "a dot segment", path: "./docs/policy.md"},
		{name: "an empty path", path: "", wantErr: true},
		{name: "an absolute path", path: "/etc/passwd", wantErr: true},
		{name: "a parent segment", path: "../secret.md", wantErr: true},
		{name: "a nested parent segment", path: "docs/../../secret.md", wantErr: true},
		{name: "a backslash parent segment", path: `docs\..\secret.md`, wantErr: true},
		{name: "a bare parent", path: "..", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRelative(tc.path)

			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateRelative(%q) = %v, want error %v", tc.path, err, tc.wantErr)
			}
			if tc.wantErr && !fault.HasCode(err, ErrorCodeOutside) {
				t.Errorf("ValidateRelative(%q) = %v, want %s", tc.path, err, ErrorCodeOutside)
			}
		})
	}
}

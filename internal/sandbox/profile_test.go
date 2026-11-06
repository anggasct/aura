package sandbox

import (
	"testing"
	"time"
)

func TestProfileValidation(t *testing.T) {
	cases := []struct {
		name    string
		profile Profile
		valid   bool
	}{
		{"base", ProfileBase, true},
		{"connector", ProfileConnector, true},
		{"unknown", Profile("admin"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.profile.validate()
			if tc.valid {
				if err != nil {
					t.Fatalf("validate = %v, want nil", err)
				}
				return
			}
			if code, ok := CodeOf(err); !ok || code != ErrorCodeInvalidArgument {
				t.Fatalf("validate = %v, want invalid_argument", err)
			}
		})
	}
}

func TestUnknownProfileFailsClosedOnEveryEntryPoint(t *testing.T) {
	spec := Spec{Profile: Profile("admin"), WorkingDir: t.TempDir(), Limits: Limits{Timeout: time.Second}}
	if _, err := Run(t.Context(), &spec, "true"); !hasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("Run = %v, want invalid_argument", err)
	}
	req := testSessionRequest(t)
	req.Profile = Profile("admin")
	if _, err := Start(t.Context(), req); !hasCode(err, ErrorCodeInvalidArgument) {
		t.Fatalf("Start = %v, want invalid_argument", err)
	}
}

func TestConnectorProfileShapesTheFilesystem(t *testing.T) {
	if ProfileBase.workingDirReadOnly() || ProfileBase.scratchDir() {
		t.Error("the base profile keeps a writable working directory and no scratch directory")
	}
	if !ProfileConnector.workingDirReadOnly() || !ProfileConnector.scratchDir() {
		t.Error("the connector profile keeps its bundle read-only and gets a scratch directory")
	}
}

func hasCode(err error, want ErrorCode) bool {
	code, ok := CodeOf(err)
	return ok && code == want
}

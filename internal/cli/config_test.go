package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigCheckReportsOKAndFailures(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("AURA_CONFIG", "")
	twoProblems := filepath.Join(t.TempDir(), "two.yaml")
	if err := os.WriteFile(twoProblems, []byte("version: 1\nserver:\n  typo: true\nlogging:\n  other: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "absent", "config.yaml")
	nestedRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(nestedRoot, "nested.yaml")
	data := filepath.Join(nestedRoot, "data")
	workspace := filepath.Join(data, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("version: 1\ntools:\n  workspace: "+workspace+"\nstorage:\n  path: "+data+"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cases := []struct {
		name     string
		path     string
		wantCode int
		wantOut  string
		wantErr  []string
	}{
		{name: "valid", path: "../../testdata/config/valid.yaml", wantCode: 0, wantOut: "config ok: valid.yaml\n"},
		{name: "invalid", path: twoProblems, wantCode: 1, wantErr: []string{"server.typo (line 3, column 3): config_unknown_key", "logging.other (line 5, column 3): config_unknown_key"}},
		{name: "workspace inside the data directory", path: nested, wantCode: 1, wantErr: []string{"tools.workspace (line 3, column 14): config_invalid", workspace, data, "set tools.workspace to a directory outside the data directory"}},
		{name: "explicit path missing", path: missing, wantCode: 1, wantErr: []string{"file not found: " + missing}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"config", "check", "--config", tc.path}
			out, err := execute(t, args...)
			if code := exitCodeOf(t, args...); code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (err %v)", code, tc.wantCode, err)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("config check: %v", err)
				}
				if !strings.Contains(out, tc.wantOut) {
					t.Fatalf("output = %q, want it to contain %q", out, tc.wantOut)
				}
				return
			}
			if err == nil {
				t.Fatal("config check succeeded, want a failure")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q:\n%v", want, err)
				}
			}
		})
	}

	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an explicit missing path was created: stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory of an explicit missing path was created: stat err = %v", err)
	}
}

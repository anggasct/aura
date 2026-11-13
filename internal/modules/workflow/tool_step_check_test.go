package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/durable/durabletest"
)

func refuseBadPath(_ string, args json.RawMessage) error {
	if strings.Contains(string(args), `"bad"`) {
		return errors.New("path is not allowed")
	}
	return nil
}

func specWithPath(id, path string) *Spec {
	spec := toolSpec(id)
	spec.Steps[0].Executor.ToolArgs = json.RawMessage(`{"path":"` + path + `"}`)
	return spec
}

func freshInterpreter(t *testing.T) *Interpreter {
	t.Helper()
	return NewInterpreter(newTestStore(t), durabletest.NewFake(), &Options{})
}

func TestCompileChecksEveryToolStepsArguments(t *testing.T) {
	tests := []struct {
		name    string
		check   func(string, json.RawMessage) error
		path    string
		wantErr bool
	}{
		{name: "accepted arguments compile", check: refuseBadPath, path: "a"},
		{name: "refused arguments fail", check: refuseBadPath, path: "bad", wantErr: true},
		{name: "a missing checker refuses every tool step", check: nil, path: "a", wantErr: true},
	}
	entries := []struct {
		name string
		run  func(t *testing.T, spec *Spec, deps *ValidationDeps) error
	}{
		{name: "Compile", run: func(_ *testing.T, spec *Spec, deps *ValidationDeps) error {
			_, err := Compile(spec, deps)
			return err
		}},
		{name: "Install", run: func(t *testing.T, spec *Spec, deps *ValidationDeps) error {
			return freshInterpreter(t).Install(t.Context(), []*Spec{spec}, deps)
		}},
		{name: "CompileProposal", run: func(t *testing.T, spec *Spec, deps *ValidationDeps) error {
			interpreter := freshInterpreter(t)
			if err := interpreter.LoadAll(t.Context(), nil, deps); err != nil {
				t.Fatalf("LoadAll: %v", err)
			}
			_, err := interpreter.CompileProposal(NormalizeProposal(spec, "prop_check"), "main")
			return err
		}},
	}
	for _, entry := range entries {
		for _, tc := range tests {
			t.Run(entry.name+"/"+tc.name, func(t *testing.T) {
				deps := testValidationDeps()
				deps.CheckToolCall = tc.check
				err := entry.run(t, specWithPath("checked", tc.path), deps)
				if !tc.wantErr {
					if err != nil {
						t.Fatalf("%s = %v, want nil", entry.name, err)
					}
					return
				}
				if !hasCode(err, ErrorCodeSpecInvalid) {
					t.Fatalf("%s = %v, want %s", entry.name, err, ErrorCodeSpecInvalid)
				}
				if !strings.Contains(err.Error(), `"read"`) {
					t.Fatalf("%s error does not name the step: %v", entry.name, err)
				}
			})
		}
	}
}

func TestBootQuarantinesARoutineThatNoLongerCompiles(t *testing.T) {
	disk := newTestStore(t)
	good, broken := specWithPath("good_routine", "a"), specWithPath("broken_routine", "bad")
	author := NewInterpreter(disk, durabletest.NewFake(), &Options{})
	if err := author.LoadAll(t.Context(), []*Spec{good, broken}, testValidationDeps()); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	refusing := testValidationDeps()
	refusing.CheckToolCall = refuseBadPath

	t.Run("LoadRoutines loads the rest and returns the failing routine", func(t *testing.T) {
		interpreter := NewInterpreter(disk, durabletest.NewFake(), &Options{})
		quarantined, err := interpreter.LoadRoutines(t.Context(), []*Spec{good, broken}, refusing)
		if err != nil {
			t.Fatalf("LoadRoutines: %v", err)
		}
		if len(quarantined) != 1 || quarantined[0].ID != broken.ID || quarantined[0].Err == nil {
			t.Fatalf("quarantined = %+v, want only %s", quarantined, broken.ID)
		}
		if _, ok := interpreter.Spec(good.ID); !ok {
			t.Fatal("the compiling routine was not loaded")
		}
		if _, ok := interpreter.Spec(broken.ID); ok {
			t.Fatal("the quarantined routine was loaded")
		}
		if _, err := interpreter.Start(t.Context(), broken.ID, nil, ownerOrigin()); !hasCode(err, ErrorCodeDefinitionNotFound) {
			t.Fatalf("Start of a quarantined routine = %v, want %s", err, ErrorCodeDefinitionNotFound)
		}
	})

	t.Run("LoadAll and Install still fail as a batch", func(t *testing.T) {
		for name, load := range map[string]func(*Interpreter) error{
			"LoadAll": func(i *Interpreter) error { return i.LoadAll(t.Context(), []*Spec{good, broken}, refusing) },
			"Install": func(i *Interpreter) error { return i.Install(t.Context(), []*Spec{good, broken}, refusing) },
		} {
			interpreter := freshInterpreter(t)
			if err := load(interpreter); !hasCode(err, ErrorCodeSpecInvalid) {
				t.Fatalf("%s = %v, want %s", name, err, ErrorCodeSpecInvalid)
			}
			if _, ok := interpreter.Spec(good.ID); ok {
				t.Fatalf("%s loaded part of a failing batch", name)
			}
		}
	})
}

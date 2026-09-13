package terminal

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeSkills struct {
	context SkillContext
	err     error
	name    string
}

func (f *fakeSkills) ActivateSkill(_ context.Context, name string) (SkillContext, error) {
	f.name = name
	if f.err != nil {
		return SkillContext{}, f.err
	}
	return f.context, nil
}

func runSkillCommand(t *testing.T, skills Skills, input string) (out, diag string) {
	t.Helper()
	console, stdout, stderr, cancel := newConsoleForTest(&fakeRunner{eventsFor: func(string) []Event { return nil }}, newFakeSessions(), input)
	defer cancel()
	console.SetSkills(skills)
	if err := console.Run(t.Context()); err != nil {
		t.Fatalf("run: %v", err)
	}
	return stdout.String(), stderr.String()
}

func TestSkillCommandActivates(t *testing.T) {
	activated := SkillContext{ID: "local/pdf@0123456789ab", Caveat: "untrusted", Text: "Do things."}
	fake := &fakeSkills{context: activated}
	out, _ := runSkillCommand(t, fake, "/skill pdf\n")
	if fake.name != "pdf" {
		t.Errorf("name = %q", fake.name)
	}
	for _, want := range []string{"skill local/pdf@0123456789ab", "untrusted", "Do things."} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q: %q", want, out)
		}
	}
}

func TestSkillCommandFailsClosed(t *testing.T) {
	_, diag := runSkillCommand(t, &fakeSkills{err: errors.New("skill_unavailable: skill is not available")}, "/skill missing\n")
	if !strings.Contains(diag, "skill missing") {
		t.Errorf("diag = %q", diag)
	}
	_, diag = runSkillCommand(t, nil, "/skill pdf\n")
	if !strings.Contains(diag, "not available") {
		t.Errorf("diag = %q", diag)
	}
	_, diag = runSkillCommand(t, &fakeSkills{}, "/skill\n")
	if !strings.Contains(diag, "usage: /skill <name>") {
		t.Errorf("diag = %q", diag)
	}
}

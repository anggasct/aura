package terminal

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeSkills struct {
	context     SkillContext
	draft       SkillDraft
	err         error
	name        string
	draftName   string
	draftDesc   string
	createCalls int
}

func (f *fakeSkills) ActivateSkill(_ context.Context, name string) (SkillContext, error) {
	f.name = name
	if f.err != nil {
		return SkillContext{}, f.err
	}
	return f.context, nil
}

func (f *fakeSkills) CreateSkill(_ context.Context, name, description string) (SkillDraft, error) {
	f.createCalls++
	f.draftName, f.draftDesc = name, description
	if f.err != nil {
		return SkillDraft{}, f.err
	}
	return f.draft, nil
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

func TestSkillCreateCommandImmediate(t *testing.T) {
	fake := &fakeSkills{draft: SkillDraft{ID: "local/n@0123456789ab", Digest: "0123456789abcdef"}}
	out, _ := runSkillCommand(t, fake, "/skill-create n A new skill.\n")
	if fake.createCalls != 1 || fake.draftName != "n" || fake.draftDesc != "A new skill." {
		t.Errorf("create = %+v", fake)
	}
	for _, want := range []string{"draft local/n@0123456789ab", "digest 0123456789abcdef", "aura skills review"} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q: %q", want, out)
		}
	}
}

func TestSkillCreateCommandPrompts(t *testing.T) {
	fake := &fakeSkills{draft: SkillDraft{ID: "local/n@0123456789ab", Digest: "0123456789abcdef"}}
	out, _ := runSkillCommand(t, fake, "/skill-create n\nA prompted description.\n")
	if fake.createCalls != 1 || fake.draftDesc != "A prompted description." {
		t.Errorf("create = %+v", fake)
	}
	if !strings.Contains(out, "describe the skill") || !strings.Contains(out, "draft local/n@0123456789ab") {
		t.Errorf("out = %q", out)
	}
}

func TestSkillCreateCommandAborts(t *testing.T) {
	fake := &fakeSkills{}
	out, _ := runSkillCommand(t, fake, "/skill-create n\n/status\n")
	if fake.createCalls != 0 {
		t.Errorf("slash input should abort pending creation")
	}
	if strings.Contains(out, "draft ") {
		t.Errorf("out = %q", out)
	}
	_, diag := runSkillCommand(t, nil, "/skill-create n A skill.\n")
	if !strings.Contains(diag, "not available") {
		t.Errorf("diag = %q", diag)
	}
	_, diag = runSkillCommand(t, &fakeSkills{}, "/skill-create\n")
	if !strings.Contains(diag, "usage: /skill-create") {
		t.Errorf("diag = %q", diag)
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

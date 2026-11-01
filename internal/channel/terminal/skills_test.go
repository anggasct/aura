package terminal

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/command"
)

type fakeSkills struct {
	pin         command.SkillPin
	draft       command.Draft
	err         error
	name        string
	draftName   string
	draftDesc   string
	createCalls int
}

func (f *fakeSkills) Activate(_ context.Context, name string) (command.SkillPin, error) {
	f.name = name
	if f.err != nil {
		return command.SkillPin{}, f.err
	}
	return f.pin, nil
}

func (f *fakeSkills) StageDraft(_ context.Context, name, description string) (command.Draft, error) {
	f.createCalls++
	f.draftName, f.draftDesc = name, description
	if f.err != nil {
		return command.Draft{}, f.err
	}
	return f.draft, nil
}

func runSkillCommand(t *testing.T, skills command.Skills, input string) (out, diag string) {
	t.Helper()
	out, diag, _ = runSkillTurns(t, skills, input)
	return out, diag
}

func runSkillTurns(t *testing.T, skills command.Skills, input string) (out, diag string, calls []Request) {
	t.Helper()
	runner := &fakeRunner{eventsFor: func(string) []Event {
		return []Event{{Kind: "message.completed", Payload: []byte(`{"text":"done"}`)}, {Kind: "turn.completed"}}
	}}
	console, stdout, stderr, cancel := newConsoleForTest(runner, newFakeSessions(), input)
	defer cancel()
	console.SetSkills(skills)
	if err := console.Run(t.Context()); err != nil {
		t.Fatalf("run: %v", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return stdout.String(), stderr.String(), slices.Clone(runner.calls)
}

func TestSkillCommandStartsAPinnedTurn(t *testing.T) {
	pin := command.SkillPin{SkillID: "local/pdf@0123456789ab", Digest: "0123456789abcdef", InvocationID: "inv_1", PolicyVersion: "1"}
	fake := &fakeSkills{pin: pin}
	out, _, calls := runSkillTurns(t, fake, "/skill pdf check the auth module\nthanks\n")
	if fake.name != "pdf" {
		t.Errorf("name = %q", fake.name)
	}
	if len(calls) != 2 {
		t.Fatalf("turns = %d, want 2", len(calls))
	}
	if calls[0].Skill == nil || *calls[0].Skill != pin || calls[0].Parts[0].Text != "/skill pdf check the auth module" {
		t.Errorf("skill turn = %+v", calls[0])
	}
	if calls[1].Skill != nil || calls[1].Parts[0].Text != "thanks" {
		t.Errorf("follow-up turn = %+v", calls[1])
	}
	if strings.Contains(out, "skill local/pdf") || !strings.Contains(out, "done") {
		t.Errorf("out = %q", out)
	}
}

func TestSkillCommandWithoutRequestStartsATurn(t *testing.T) {
	fake := &fakeSkills{pin: command.SkillPin{SkillID: "local/pdf@0123456789ab", Digest: "d", InvocationID: "i", PolicyVersion: "1"}}
	_, _, calls := runSkillTurns(t, fake, "/skill pdf\n")
	if len(calls) != 1 || calls[0].Parts[0].Text != "/skill pdf" || calls[0].Skill == nil {
		t.Errorf("turns = %+v", calls)
	}
}

func TestSkillCreateCommandImmediate(t *testing.T) {
	fake := &fakeSkills{draft: command.Draft{ID: "local/n@0123456789ab", Digest: "0123456789abcdef"}}
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
	fake := &fakeSkills{draft: command.Draft{ID: "local/n@0123456789ab", Digest: "0123456789abcdef"}}
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

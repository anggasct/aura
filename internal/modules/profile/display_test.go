package profile_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/profile/profiletest"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/tools"
)

func (r *opsRig) cli(t *testing.T, args ...string) string {
	t.Helper()
	open := func(_ *cobra.Command, _ *operation.Descriptor, run func(context.Context, operation.Invoker) error) error {
		return run(t.Context(), r.Registry)
	}
	roots, err := operation.Commands([]operation.Catalog{profile.Catalog()}, open)
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	var out bytes.Buffer
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(roots...)
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("aura %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func (r *opsRig) propose(t *testing.T, key, value string) string {
	t.Helper()
	fact, err := r.profile.Service.ProposeAgentFact(t.Context(), &profile.AgentProposal{
		OwnerID: principal.OwnerID, Category: "tool", Key: key, Value: value,
	}, profiletest.SeededAt)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	return fact.ID
}

func widest(char string, count int) string { return strings.Repeat(char, count) }

func TestAnAgentFactAtTheLongestSizeIsShownWholeByReviewShowAndTheCard(t *testing.T) {
	cases := map[string]string{
		"four byte letters": widest("\U0001D49C", profile.AgentValueChars),
		"backslashes":       widest(`\`, profile.AgentValueChars),
		"quotes":            widest(`"`, profile.AgentValueChars),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			rig := newOpsRig(t)
			key := widest("\U0001D49C", profile.AgentKeyChars)
			id := rig.propose(t, key, value)

			review := rig.cli(t, "profile", "review")
			show := rig.cli(t, "profile", "show", id)
			outcome := rig.tool("profile.delete", `{"id":"`+id+`"}`)

			requireOK(t, outcome)
			wantText := tools.EscapeDisplayText(value)
			for screen, text := range map[string]string{"review": review, "show": show} {
				if !strings.Contains(text, wantText) || !strings.Contains(text, key) || !strings.Contains(text, "agent") {
					t.Errorf("%s does not show the whole fact with its origin:\n%s", screen, text)
				}
			}
			cards := rig.Prompts()
			if len(cards) != 1 {
				t.Fatalf("cards = %d, want 1", len(cards))
			}
			card := cards[0].Arguments
			if len(card) > tools.ApprovalDisplayLimit {
				t.Errorf("the card is %d bytes, over %d", len(card), tools.ApprovalDisplayLimit)
			}
			if !strings.Contains(card, tools.EscapeDisplayText(`"`+escapedQuote(value)+`"`)) || !strings.Contains(card, key) {
				t.Errorf("the card hides part of the fact:\n%s", card)
			}
		})
	}
}

func escapedQuote(value string) string {
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
	return quoted
}

func TestTheAgentCannotWriteWhatTheOwnerCouldNotReadWhole(t *testing.T) {
	cases := []struct{ name, key, value string }{
		{name: "a value over the agent bound", key: "editor", value: widest("a", profile.AgentValueChars+1)},
		{name: "a key over the agent bound", key: widest("k", profile.AgentKeyChars+1), value: "neovim"},
		{name: "a control character", key: "editor", value: `line\u0007bell`},
		{name: "a line break", key: "editor", value: `one\ntwo`},
		{name: "a control character in the key", key: `ed\u0007itor`, value: "neovim"},
		{name: "a direction override", key: "editor", value: `evil\u202etxt`},
		{name: "a non printing format mark", key: "editor", value: `zero\u200bwidth`},
		{name: "a private use character", key: "editor", value: `\ue000`},
		{name: "a line separator", key: "editor", value: `split\u2028here`},
		{name: "a variation selector", key: "editor", value: `mark\ufe0f`},
		{name: "a prompt marker", key: "editor", value: "[untrusted: notes]"},
		{name: "a prompt marker close", key: "editor", value: "[/untrusted]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOpsRig(t)
			input := `{"category":"tool","key":"` + tc.key + `","value":"` + tc.value + `"}`

			outcome := rig.tool("profile.set", input)

			if outcome.Class == toolbroker.ResultOK {
				t.Fatalf("the agent stored %s: %+v", tc.name, outcome)
			}
			facts, err := rig.profile.Service.ListFacts(t.Context(), principal.OwnerID, "", "", 10)
			if err != nil || len(facts) != 0 {
				t.Errorf("facts = %v (%v), want none stored", facts, err)
			}
		})
	}
}

func TestTheOwnerKeepsTheFullFactLength(t *testing.T) {
	rig := newOpsRig(t)
	value := widest("v", 300)

	if _, err := rig.invoke(t, operation.SurfaceCLI, "profile.set", `{"category":"tool","key":"editor","value":"`+value+`"}`); err != nil {
		t.Fatalf("set: %v", err)
	}
	if shown := rig.cli(t, "profile", "list"); !strings.Contains(shown, value) {
		t.Errorf("list cuts the value:\n%s", shown)
	}
}

func TestTheAgentSchemaCarriesTheAgentBounds(t *testing.T) {
	rig := newOpsRig(t)
	for _, registration := range rig.Registry.Tools() {
		if registration.Definition.Name != operation.ToolName("profile.set") {
			continue
		}
		schema := string(registration.Definition.Schema)
		for _, want := range []string{`"maxLength":32`, `"maxLength":200`} {
			if !strings.Contains(schema, want) {
				t.Errorf("the tool schema lacks %s: %s", want, schema)
			}
		}
		return
	}
	t.Fatal("the profile.set tool is not registered")
}

func TestAnEmptyDeletePreviewIsNothingToDoOnEverySurface(t *testing.T) {
	rig := newOpsRig(t)
	rig.seed(t)
	id := profiletest.VerifiedFactID()
	input := `{"id":"` + id + `"}`
	if _, err := rig.apply(t, operation.SurfaceCLI, "profile.delete", input); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	for _, surface := range []operation.Surface{operation.SurfaceCLI, operation.SurfaceChat} {
		preview, err := rig.Registry.Preview(t.Context(), "profile.delete", rig.call(surface, "profile.delete", input))
		if err != nil {
			t.Fatalf("%s preview: %v", surface, err)
		}
		if !preview.Empty || len(preview.Lines) != 1 || preview.Lines[0] != "nothing to delete" {
			t.Errorf("%s preview = %+v, want nothing to delete", surface, preview)
		}
	}
	if out := rig.cli(t, "profile", "delete", id, "--apply"); strings.TrimSpace(out) != "nothing to delete" {
		t.Errorf("the CLI printed %q for an already deleted fact", out)
	}
	outcome := rig.tool("profile.delete", input)
	requireOK(t, outcome)
	if len(rig.Prompts()) != 0 {
		t.Error("a card was raised for a fact that is already deleted")
	}
}

func TestAProposalAnswersWithTheStoredState(t *testing.T) {
	const input = `{"category":"language","key":"backend","value":"Go"}`
	cases := []struct {
		name  string
		seed  func(*testing.T, *opsRig)
		wants string
		not   string
	}{
		{name: "a new proposal", seed: func(*testing.T, *opsRig) {}, wants: "proposed: ", not: "stored"},
		{name: "an owner-verified fact", seed: func(t *testing.T, rig *opsRig) {
			if err := rig.profile.SeedVerified(t.Context()); err != nil {
				t.Fatal(err)
			}
		}, wants: "owner-verified", not: "proposed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOpsRig(t)
			tc.seed(t, rig)

			outcome := rig.tool("profile.set", input)

			requireOK(t, outcome)
			text := resultText(t, outcome.Output)
			if !strings.Contains(text, tc.wants) || strings.Contains(text, tc.not) {
				t.Errorf("the agent was told %q, want %q and not %q", text, tc.wants, tc.not)
			}
		})
	}
}

func TestAnOwnerSetOfAnAgentValueWinsOverTheCandidateAndTheRejection(t *testing.T) {
	for _, prior := range []string{profile.StatusCandidate, profile.StatusRejected} {
		t.Run(prior, func(t *testing.T) {
			rig := newOpsRig(t)
			id := rig.propose(t, "editor", "neovim")
			if prior == profile.StatusRejected {
				if _, err := rig.invoke(t, operation.SurfaceCLI, "profile.reject", `{"id":"`+id+`"}`); err != nil {
					t.Fatalf("reject: %v", err)
				}
			}

			if _, err := rig.invoke(t, operation.SurfaceCLI, "profile.set", `{"category":"tool","key":"editor","value":"neovim"}`); err != nil {
				t.Fatalf("owner set after %s: %v", prior, err)
			}

			fact := rig.fact(t, id)
			if !fact.OwnerVerified || fact.Status != profile.StatusActive || fact.ConflictsWithID != "" || fact.Confidence != 1.0 {
				t.Errorf("fact = %+v, want an owner-verified active fact with no conflict", fact)
			}
			if !strings.Contains(rig.ownerContext(t), "neovim") {
				t.Error("the owner value is missing from the owner context")
			}
		})
	}
}

func TestAnOwnerSetKeepsTheRequestedExpiryOfAPromotedValue(t *testing.T) {
	rig := newOpsRig(t)
	id := rig.propose(t, "editor", "neovim")

	if _, err := rig.invoke(t, operation.SurfaceCLI, "profile.set", `{"category":"tool","key":"editor","value":"neovim","expires_at":"2099-01-01T00:00:00Z"}`); err != nil {
		t.Fatalf("set: %v", err)
	}

	if fact := rig.fact(t, id); fact.ExpiresAt == nil || fact.ExpiresAt.Year() != 2099 {
		t.Errorf("expiry = %v, want 2099", fact.ExpiresAt)
	}
}

func TestUntrustedProfileValuesAreEscapedOnTheCLI(t *testing.T) {
	rig := newOpsRig(t)
	value := "red\u001b[31m \u202egnp.exe\a"
	if _, err := rig.profile.Service.SetFact(t.Context(), principal.OwnerID, "tool", "editor", value, nil, profiletest.SeededAt); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := profile.FactID(principal.OwnerID, "tool", "editor", profile.ValueDigest(value))

	for _, args := range [][]string{{"profile", "list"}, {"profile", "show", id}, {"profile", "delete", id}} {
		out := rig.cli(t, args...)
		for _, raw := range []string{"\u001b", "\u202e", "\a"} {
			if strings.Contains(out, raw) {
				t.Errorf("aura %s printed a raw %q:\n%s", strings.Join(args, " "), raw, out)
			}
		}
		if !strings.Contains(out, `\u{202e}`) {
			t.Errorf("aura %s did not show the escaped direction control:\n%s", strings.Join(args, " "), out)
		}
	}
}

func TestAcceptEchoesTheWholeValueItVerified(t *testing.T) {
	rig := newOpsRig(t)
	value := widest(`a\`, profile.AgentValueChars/2)
	id := rig.propose(t, "editor", value)

	out := rig.cli(t, "profile", "accept", id)

	if !strings.Contains(out, tools.EscapeDisplayText(value)) || !strings.Contains(out, id) {
		t.Errorf("accept did not echo the whole value:\n%s", out)
	}
}

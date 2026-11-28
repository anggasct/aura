package operation_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
)

var codePattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)

func newConfirmer(t *testing.T) (*operation.Confirmer, string) {
	t.Helper()
	dir := t.TempDir()
	confirmer, err := operation.NewConfirmer(dir)
	if err != nil {
		t.Fatalf("NewConfirmer: %v", err)
	}
	return confirmer, dir
}

func TestConfirmationCodeIsEightCrockfordCharactersOfTheKeyedDigest(t *testing.T) {
	confirmer, _ := newConfirmer(t)

	first, err := confirmer.Code("digest-a")
	if err != nil {
		t.Fatalf("Code: %v", err)
	}
	again, _ := confirmer.Code("digest-a")
	other, _ := confirmer.Code("digest-b")

	if !codePattern.MatchString(first) {
		t.Errorf("code %q is not two groups of four Crockford base32 characters", first)
	}
	if again != first || other == first {
		t.Errorf("codes = %q, %q, %q; want a stable code per digest and a different one for another digest", first, again, other)
	}
	for name, supplied := range map[string]string{
		"exact":             first,
		"without the dash":  strings.ReplaceAll(first, "-", ""),
		"lower case":        strings.ToLower(first),
		"padded with space": " " + first + " ",
	} {
		if !confirmer.Matches("digest-a", supplied) {
			t.Errorf("%s: code %q does not match its digest", name, supplied)
		}
	}
	for name, supplied := range map[string]string{
		"the code of another digest": other,
		"empty":                      "",
		"truncated":                  first[:4],
		"extended":                   first + "0",
	} {
		if confirmer.Matches("digest-a", supplied) {
			t.Errorf("%s: %q matches digest-a, want a refusal", name, supplied)
		}
	}
}

func TestConfirmKeyIsCreatedPrivate(t *testing.T) {
	confirmer, dir := newConfirmer(t)
	path := filepath.Join(dir, operation.ConfirmKeyFile)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the key exists before first use: %v", err)
	}

	code, err := confirmer.Code("digest")
	if err != nil {
		t.Fatalf("Code: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Errorf("key mode %v size %d, want 0600 and 32 bytes", info.Mode().Perm(), info.Size())
	}
	reopened, err := operation.NewConfirmer(dir)
	if err != nil {
		t.Fatalf("NewConfirmer: %v", err)
	}
	if again, err := reopened.Code("digest"); err != nil || again != code {
		t.Errorf("a second process reads code %q, %v; want the same key and code %q", again, err, code)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the directory holds %d entries after key creation, want only the key", len(entries))
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	loose, _ := operation.NewConfirmer(dir)
	if _, err := loose.Code("digest"); !fault.HasCode(err, operation.ErrorCodeFailed) {
		t.Errorf("a key readable by others gave %v, want %s", err, operation.ErrorCodeFailed)
	}
}

func TestConfirmationRejectsAnUnreadableKeyDirectory(t *testing.T) {
	if _, err := operation.NewConfirmer(""); err == nil {
		t.Error("an empty directory is accepted")
	}
	missing, err := operation.NewConfirmer(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("NewConfirmer: %v", err)
	}
	if _, err := missing.Code("digest"); !fault.HasCode(err, operation.ErrorCodeFailed) {
		t.Errorf("a missing directory gave %v, want %s", err, operation.ErrorCodeFailed)
	}
	if missing.Matches("digest", "ABCD-EFGH") {
		t.Error("a code matched without a usable key")
	}
}

type codedOperation struct {
	rig     *operationtest.Rig
	handled atomic.Int32
	version atomic.Int32
}

func codedRig(t *testing.T) (*codedOperation, []operation.Catalog) {
	t.Helper()
	coded := &codedOperation{}
	catalog := operationtest.Catalog()
	descriptor := descriptorNamed(&catalog, operationtest.Host)
	descriptor.Preview, descriptor.Confirmation = true, operation.ConfirmPreviewWithCode
	owner := descriptorNamed(&catalog, operationtest.OwnerOnly)
	owner.Risk, owner.Commit, owner.Preview, owner.Confirmation = operation.RiskTrustGrant, operation.CommitTransactional, true, operation.ConfirmPreviewWithCode
	catalogs := []operation.Catalog{catalog}
	coded.rig = operationtest.NewRig(t, &operationtest.Options{
		Catalogs: catalogs,
		Bindings: func(stubs *operationtest.Stubs) []operation.Binding {
			bindings := stubs.Bindings()
			for i := range bindings {
				switch bindings[i].ID {
				case operationtest.Host:
					bindings[i].Handle = coded.handle(false)
					bindings[i].Preview = coded.preview
				case operationtest.OwnerOnly:
					bindings[i].Handle = coded.handle(true)
					bindings[i].Preview = coded.preview
				}
			}
			return bindings
		},
	})
	return coded, catalogs
}

func (c *codedOperation) preview(_ context.Context, call *operation.Call) (operation.Preview, error) {
	var input struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(call.Input, &input); err != nil {
		return operation.Preview{}, err
	}
	digest := "subject-" + input.Subject + "-version-" + string(rune('0'+c.version.Load()))
	return operation.Preview{Lines: []string{"subject: " + input.Subject}, Digest: digest}, nil
}

func (c *codedOperation) handle(commits bool) operation.Handler {
	return func(ctx context.Context, call *operation.Call) (operation.Result, error) {
		c.handled.Add(1)
		data := json.RawMessage(`{"done":true}`)
		if !commits {
			return operation.Result{Data: data}, nil
		}
		return call.Commit(ctx, func(context.Context) (operation.Result, error) { return operation.Result{Data: data}, nil })
	}
}

func codeFrom(t *testing.T, output string) string {
	t.Helper()
	index := strings.Index(output, "--apply ")
	if index < 0 || !strings.Contains(output, "confirm with: ") {
		t.Fatalf("output %q has no confirm line", output)
	}
	code := strings.Fields(output[index+len("--apply "):])[0]
	if !codePattern.MatchString(code) {
		t.Fatalf("output %q carries %q, not a confirmation code", output, code)
	}
	return code
}

func TestTheCLIAppliesACodeConfirmedOperationOnlyWithThePreviewsCode(t *testing.T) {
	coded, catalogs := codedRig(t)
	commands, err := operation.Commands(catalogs, func(_ *cobra.Command, _ *operation.Descriptor, run func(ctx context.Context, invoker operation.Invoker) error) error {
		return run(t.Context(), coded.rig.Registry)
	})
	if err != nil {
		t.Fatalf("Commands: %v", err)
	}
	root := &cobra.Command{Use: "aura", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(commands...)
	cli := &cliRig{rig: coded.rig, root: root}

	preview, err := cli.run(t, "stub", "host", "alpha")
	if err != nil || coded.handled.Load() != 0 {
		t.Fatalf("preview = %q, %v, handled %d; want the preview only", preview, err, coded.handled.Load())
	}
	code := codeFrom(t, preview)
	if !strings.Contains(preview, "confirm with: aura stub host alpha --apply "+code) {
		t.Errorf("preview %q does not print the command with its code", preview)
	}

	for name, args := range map[string][]string{
		"a wrong code":    {"stub", "host", "alpha", "--apply", "0000-0000"},
		"an empty code":   {"stub", "host", "alpha", "--apply", ""},
		"another subject": {"stub", "host", "beta", "--apply", code},
	} {
		out, err := cli.run(t, args...)
		var usage *clikit.UsageError
		if !fault.HasCode(err, operation.ErrorCodeConfirmation) || !errors.As(err, &usage) || coded.handled.Load() != 0 {
			t.Errorf("%s: output %q, error %v, handled %d; want %s as a usage error and no change", name, out, err, coded.handled.Load(), operation.ErrorCodeConfirmation)
		}
	}

	if out, err := cli.run(t, "stub", "host", "alpha", "--apply", code); err != nil || !strings.Contains(out, "done: true") || coded.handled.Load() != 1 {
		t.Fatalf("apply = %q, %v, handled %d; want one run", out, err, coded.handled.Load())
	}

	coded.version.Add(1)
	if _, err := cli.run(t, "stub", "host", "alpha", "--apply", code); !fault.HasCode(err, operation.ErrorCodeConfirmation) || coded.handled.Load() != 1 {
		t.Errorf("after the object changed, the old code gave %v, handled %d; want %s and no second run", err, coded.handled.Load(), operation.ErrorCodeConfirmation)
	}
}

func TestChatAppliesACodeConfirmedOperationOnlyWithThePreviewsCode(t *testing.T) {
	coded, _ := codedRig(t)
	chat, err := operation.NewChat(&operation.ChatDeps{Registry: coded.rig.Registry, Capabilities: capabilitytest.Host(t)})
	if err != nil {
		t.Fatalf("NewChat: %v", err)
	}

	preview := runChat(t, chat, typed{"owner-only alpha", "telegram:1"})
	if preview.Code != command.ErrorCodeOK || coded.handled.Load() != 0 {
		t.Fatalf("preview = %+v, handled %d; want the preview only", preview, coded.handled.Load())
	}
	code := codeFrom(t, strings.Join(preview.Lines, "\n"))
	if want := "confirm with: /stub owner-only alpha --apply " + code; !slices.Contains(preview.Lines, want) {
		t.Errorf("preview lines %q lack %q", preview.Lines, want)
	}

	wrong := runChat(t, chat, typed{"owner-only alpha --apply 0000-0000", "telegram:2"})
	if wrong.Code != command.ErrorCodeUsage || !strings.HasPrefix(wrong.Lines[0], "operation_confirmation_invalid") || coded.handled.Load() != 0 {
		t.Fatalf("wrong code = %+v handled %d; want operation_confirmation_invalid and no change", wrong, coded.handled.Load())
	}
	plan := chat.Plan(operationtest.Group, tokens(t, "owner-only alpha --apply"))
	if plan.Refusal == nil || plan.Refusal.Code != command.ErrorCodeUsage {
		t.Errorf("apply without a value planned %+v, want a usage refusal", plan)
	}

	applied := runChat(t, chat, typed{"owner-only alpha --apply " + code, "telegram:3"})
	if applied.Code != command.ErrorCodeOK || coded.handled.Load() != 1 {
		t.Fatalf("applied = %+v handled %d; want one run", applied, coded.handled.Load())
	}

	coded.version.Add(1)
	stale := runChat(t, chat, typed{"owner-only alpha --apply=" + code, "telegram:4"})
	if stale.Code != command.ErrorCodeUsage || coded.handled.Load() != 1 {
		t.Errorf("after the object changed, the old code gave %+v handled %d; want a refusal and no second run", stale, coded.handled.Load())
	}
}

func TestACodeConfirmedOperationIsNeverAnAgentTool(t *testing.T) {
	catalog := operationtest.Catalog()
	grant := descriptorNamed(&catalog, operationtest.Grant)
	grant.Confirmation = operation.ConfirmPreviewWithCode
	if err := operation.ValidateCatalogs([]operation.Catalog{catalog}); err == nil || !fault.HasCode(err, operation.ErrorCodeInvalid) {
		t.Errorf("an agent operation confirmed with a code gave %v, want %s", err, operation.ErrorCodeInvalid)
	}
}

func TestARegistryWithACodeConfirmedOperationNeedsTheConfirmationKey(t *testing.T) {
	catalog := operationtest.Catalog()
	host := descriptorNamed(&catalog, operationtest.Host)
	host.Preview, host.Confirmation = true, operation.ConfirmPreviewWithCode
	rig := operationtest.NewRig(t, &operationtest.Options{
		Catalogs:         []operation.Catalog{catalog},
		AllowInvalid:     true,
		WithoutConfirmer: true,
		Bindings: func(stubs *operationtest.Stubs) []operation.Binding {
			bindings := stubs.Bindings()
			for i := range bindings {
				if bindings[i].ID == operationtest.Host {
					bindings[i].Preview = bindings[slices.IndexFunc(bindings, func(b operation.Binding) bool { return b.ID == operationtest.Delete })].Preview
				}
			}
			return bindings
		},
	})
	if rig.RegistryErr == nil || !fault.HasCode(rig.RegistryErr, operation.ErrorCodeInvalid) {
		t.Errorf("registry error = %v, want %s for a missing confirmation key", rig.RegistryErr, operation.ErrorCodeInvalid)
	}
}

func TestTaintedGrantDeclarationsAreCheckedAtRegistration(t *testing.T) {
	tests := map[string]struct {
		id     string
		grant  operation.TaintedGrant
		valid  bool
		mutate func(d *operation.Descriptor)
	}{
		"labels on an agent trust grant": {id: operationtest.Grant, grant: operation.TaintedGrantLabels, valid: true},
		"inert on an agent trust grant":  {id: operationtest.Grant, grant: operation.TaintedGrantInert, valid: true},
		"no declaration":                 {id: operationtest.Grant, valid: true},
		"an unknown value":               {id: operationtest.Grant, grant: "sometimes"},
		"on a change":                    {id: operationtest.Change, grant: operation.TaintedGrantLabels},
		"on a read":                      {id: operationtest.Read, grant: operation.TaintedGrantInert},
		"on an owner operation": {id: operationtest.OwnerOnly, grant: operation.TaintedGrantLabels, mutate: func(d *operation.Descriptor) {
			d.Risk, d.Commit = operation.RiskTrustGrant, operation.CommitTransactional
		}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			catalog := operationtest.Catalog()
			descriptor := descriptorNamed(&catalog, tc.id)
			descriptor.TaintedGrant = tc.grant
			if tc.mutate != nil {
				tc.mutate(descriptor)
			}
			err := operation.ValidateCatalogs([]operation.Catalog{catalog})
			if tc.valid && err != nil {
				t.Fatalf("ValidateCatalogs = %v, want valid", err)
			}
			if !tc.valid && !fault.HasCode(err, operation.ErrorCodeInvalid) {
				t.Fatalf("ValidateCatalogs = %v, want %s", err, operation.ErrorCodeInvalid)
			}
		})
	}
}

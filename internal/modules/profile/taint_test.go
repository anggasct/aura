package profile_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/anggasct/aura/internal/modules/profile"
	"github.com/anggasct/aura/internal/modules/profile/profiletest"
	"github.com/anggasct/aura/internal/platform/capability/capabilitytest"
	"github.com/anggasct/aura/internal/platform/clikit"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/operation/operationtest"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/secret"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

var profileConfirmCodePattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$`)

func (r *opsRig) cliRaw(t *testing.T, args ...string) (string, error) {
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
	err = root.ExecuteContext(t.Context())
	return out.String(), err
}

func extractConfirmCode(t *testing.T, text string) string {
	t.Helper()
	idx := strings.Index(text, "--apply ")
	if idx < 0 || !strings.Contains(text, "confirm with: ") {
		t.Fatalf("preview %q has no confirm line", text)
	}
	code := strings.Fields(text[idx+len("--apply "):])[0]
	if !profileConfirmCodePattern.MatchString(code) {
		t.Fatalf("extracted %q is not a confirmation code from %q", code, text)
	}
	return code
}

type staticProfileModelCaller struct {
	text string
}

func (c staticProfileModelCaller) Extract(_ context.Context, _ *profile.ExtractRequest) (*profile.ExtractResult, error) {
	return &profile.ExtractResult{
		Text:     c.text,
		Producer: profile.ModelProducer{Provider: "test", Model: "extract-v1", ModelVersion: "1"},
	}, nil
}

func TestProfileFactProposedWhileTaintedShowsItsSource(t *testing.T) {
	const sourceLabel = "a web_fetch result from example.com"

	t.Run("agent proposal carries taint_source in review and show", func(t *testing.T) {
		rig := newOpsRigAnswering(t, func(context.Context, *toolbroker.ApprovalPrompt) (bool, error) { return true, nil })
		req := operationtest.RequestFor(
			operation.ToolName("profile.set"),
			json.RawMessage(`{"category":"preference","key":"editor","value":"emacs"}`),
			"tainted-set-1",
		)
		req.Tainted = true
		req.TaintMark = turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch example.com"}
		outcome := rig.Execute(t.Context(), req)
		requireOK(t, outcome)

		factID := profile.FactID(principal.OwnerID, "preference", "editor", profile.ValueDigest("emacs"))
		fact := rig.fact(t, factID)
		if fact.Origin != profile.OriginAgent || fact.OwnerVerified || fact.TaintSource != sourceLabel {
			t.Fatalf("fact = %+v, want unverified agent fact with taint_source %q", fact, sourceLabel)
		}

		reviewOut := rig.cli(t, "profile", "review")
		if !strings.Contains(reviewOut, factID) || !strings.Contains(reviewOut, sourceLabel) {
			t.Errorf("profile review missing taint_source:\n%s", reviewOut)
		}

		showOut := rig.cli(t, "profile", "show", factID)
		if !strings.Contains(showOut, "taint_source: "+sourceLabel) {
			t.Errorf("profile show missing taint_source:\n%s", showOut)
		}

		cleanID := rig.propose(t, "shell", "fish")
		cleanShow := rig.cli(t, "profile", "show", cleanID)
		if strings.Contains(cleanShow, "taint_source:") {
			t.Errorf("untainted fact show unexpectedly printed taint_source:\n%s", cleanShow)
		}
	})

	t.Run("extractor records taint_source from session marks at evidence sequence", func(t *testing.T) {
		ctx := t.Context()
		db := operationtest.OpenDatabase(t, profile.Schema())
		sessions := turnstore.NewSessionService(db)
		events := turnstore.NewEventStore(db)
		now := time.Now().UTC()

		if err := sessions.Create(ctx, &turn.Session{ID: "sess-extract", OwnerID: principal.OwnerID, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("Create session: %v", err)
		}
		evCleanSeq, err := events.AppendSequenced(ctx, "sess-extract", &turn.Event{
			ID: "ev-clean", SessionID: "sess-extract", TurnID: "t1", Author: "user",
			Kind: "adk_event", SchemaVersion: 1, Payload: json.RawMessage(`{"text":"i prefer tea"}`), CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("append clean event: %v", err)
		}
		markPayload, _ := turn.EncodeTaintMark(turn.TaintMark{Source: turn.TaintSourceToolResult, Detail: "web_fetch example.com"})
		if _, err := events.AppendSequenced(ctx, "sess-extract", &turn.Event{
			ID: "ev-mark", SessionID: "sess-extract", TurnID: "t2", Author: "system",
			Kind: turn.EventKindSessionTainted, SchemaVersion: 1, Payload: markPayload, CreatedAt: now,
		}); err != nil {
			t.Fatalf("append taint mark: %v", err)
		}
		evTaintedSeq, err := events.AppendSequenced(ctx, "sess-extract", &turn.Event{
			ID: "ev-dirty", SessionID: "sess-extract", TurnID: "t2", Author: "user",
			Kind: "adk_event", SchemaVersion: 1, Payload: json.RawMessage(`{"text":"i prefer espresso"}`), CreatedAt: now,
		})
		if err != nil {
			t.Fatalf("append tainted event: %v", err)
		}

		store, err := profile.NewSQLiteStore(db)
		if err != nil {
			t.Fatalf("NewSQLiteStore: %v", err)
		}
		svc, err := profile.NewService(store, profile.ServiceOptions{MinConfidence: 0.99, Scanner: secret.New()})
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		taintReader, ok := turnstore.NewTaintReader(db).(profile.SessionTaintAtReader)
		if !ok {
			t.Fatal("turnstore TaintReader does not implement SessionTaintAtReader")
		}
		extractedJSON := `[` +
			`{"category":"preference","key":"drink_clean","value":"tea","sources":[1]},` +
			`{"category":"preference","key":"drink_dirty","value":"espresso","sources":[3]}` +
			`]`
		extractor, err := profile.NewExtractor(
			svc,
			staticProfileModelCaller{text: extractedJSON},
			secret.New(),
			"profiling",
			"v1",
			256,
			4,
			slog.New(slog.DiscardHandler),
			profile.WithTaintReader(taintReader),
		)
		if err != nil {
			t.Fatalf("NewExtractor: %v", err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			extractor.Run(runCtx)
		}()
		if err := extractor.Enqueue(ctx, profile.Job{
			OwnerID:   principal.OwnerID,
			SessionID: "sess-extract",
			Events: []profile.JobEvent{
				{ID: "ev-clean", Sequence: evCleanSeq, Text: "i prefer tea"},
				{ID: "ev-dirty", Sequence: evTaintedSeq, Text: "i prefer espresso"},
			},
		}); err != nil {
			cancel()
			t.Fatalf("Enqueue: %v", err)
		}

		cleanID := profile.FactID(principal.OwnerID, "preference", "drink_clean", profile.ValueDigest("tea"))
		dirtyID := profile.FactID(principal.OwnerID, "preference", "drink_dirty", profile.ValueDigest("espresso"))
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err1 := svc.GetFact(ctx, principal.OwnerID, cleanID); err1 == nil {
				if _, err2 := svc.GetFact(ctx, principal.OwnerID, dirtyID); err2 == nil {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		<-done

		cleanFact, err := svc.GetFact(ctx, principal.OwnerID, cleanID)
		if err != nil {
			t.Fatalf("GetFact(clean): %v", err)
		}
		if cleanFact.TaintSource != "" {
			t.Errorf("clean derived fact taint_source = %q, want empty", cleanFact.TaintSource)
		}
		dirtyFact, err := svc.GetFact(ctx, principal.OwnerID, dirtyID)
		if err != nil {
			t.Fatalf("GetFact(dirty): %v", err)
		}
		if dirtyFact.TaintSource != sourceLabel {
			t.Errorf("tainted derived fact taint_source = %q, want %q", dirtyFact.TaintSource, sourceLabel)
		}
	})
}

func TestAcceptingATaintedFactNeedsTheCode(t *testing.T) {
	const taintSource = "a web_fetch result from evil.example"

	t.Run("CLI preview and code confirmation", func(t *testing.T) {
		rig := newOpsRig(t)
		taintedFact, err := rig.profile.Service.ProposeAgentFact(t.Context(), &profile.AgentProposal{
			OwnerID:     principal.OwnerID,
			Category:    "preference",
			Key:         "browser",
			Value:       "firefox",
			TaintSource: taintSource,
		}, profiletest.SeededAt)
		if err != nil {
			t.Fatalf("ProposeAgentFact: %v", err)
		}
		cleanID := rig.propose(t, "editor", "helix")

		cleanOut, err := rig.cliRaw(t, "profile", "accept", cleanID)
		if err != nil || !strings.Contains(cleanOut, "accepted: "+cleanID) {
			t.Fatalf("untainted accept = %q, %v; want immediate acceptance without code", cleanOut, err)
		}
		if got := rig.fact(t, cleanID); !got.OwnerVerified || got.TaintSource != "" {
			t.Fatalf("clean accepted fact = %+v, want verified", got)
		}

		previewOut, err := rig.cliRaw(t, "profile", "accept", taintedFact.ID)
		if err != nil {
			t.Fatalf("tainted accept preview: %v (%s)", err, previewOut)
		}
		for _, want := range []string{taintedFact.ID, "browser", "firefox", taintSource, "confirm with: aura profile accept " + taintedFact.ID + " --apply "} {
			if !strings.Contains(previewOut, want) {
				t.Errorf("preview missing %q:\n%s", want, previewOut)
			}
		}
		if got := rig.fact(t, taintedFact.ID); got.OwnerVerified || got.TaintSource != taintSource {
			t.Fatalf("preview modified fact: %+v", got)
		}
		code := extractConfirmCode(t, previewOut)

		for name, args := range map[string][]string{
			"wrong code": {"profile", "accept", taintedFact.ID, "--apply", "0000-0000"},
			"empty code": {"profile", "accept", taintedFact.ID, "--apply", ""},
		} {
			out, err := rig.cliRaw(t, args...)
			var usage *clikit.UsageError
			if !fault.HasCode(err, operation.ErrorCodeConfirmation) || !errors.As(err, &usage) {
				t.Errorf("%s: out=%q err=%v, want %s usage error", name, out, err, operation.ErrorCodeConfirmation)
			}
			if got := rig.fact(t, taintedFact.ID); got.OwnerVerified || got.TaintSource != taintSource {
				t.Errorf("%s modified fact: %+v", name, got)
			}
		}

		if _, err := rig.invoke(t, operation.SurfaceCLI, "profile.accept", `{"id":"`+taintedFact.ID+`"}`); !fault.HasCode(err, operation.ErrorCodeConfirmation) {
			t.Errorf("direct invoke without code = %v, want %s", err, operation.ErrorCodeConfirmation)
		}

		preview, err := rig.Registry.Preview(t.Context(), "profile.accept", rig.call(operation.SurfaceCLI, "profile.accept", `{"id":"`+taintedFact.ID+`"}`))
		if err != nil {
			t.Fatalf("Preview: %v", err)
		}
		if _, err := rig.DB.ExecContext(t.Context(), `UPDATE profile_fact SET taint_source = ? WHERE id = ?`, "a webhook", taintedFact.ID); err != nil {
			t.Fatalf("mutate taint_source: %v", err)
		}
		staleCall := rig.call(operation.SurfaceCLI, "profile.accept", `{"id":"`+taintedFact.ID+`"}`)
		staleCall.PreviewDigest = preview.Digest
		staleCall.Confirmation = preview.Code
		if _, err := rig.Registry.Invoke(t.Context(), "profile.accept", staleCall); !fault.HasCode(err, operation.ErrorCodePreviewStale) {
			t.Errorf("stale preview apply = %v, want %s", err, operation.ErrorCodePreviewStale)
		}
		if _, err := rig.cliRaw(t, "profile", "accept", taintedFact.ID, "--apply", code); !fault.HasCode(err, operation.ErrorCodeConfirmation) {
			t.Errorf("CLI apply with code after drift = %v, want %s", err, operation.ErrorCodeConfirmation)
		}

		freshPreview, err := rig.cliRaw(t, "profile", "accept", taintedFact.ID)
		if err != nil {
			t.Fatalf("fresh preview: %v", err)
		}
		freshCode := extractConfirmCode(t, freshPreview)
		appliedOut, err := rig.cliRaw(t, "profile", "accept", taintedFact.ID, "--apply", freshCode)
		if err != nil || !strings.Contains(appliedOut, "accepted: "+taintedFact.ID) {
			t.Fatalf("apply with valid code = %q, %v; want accepted", appliedOut, err)
		}
		accepted := rig.fact(t, taintedFact.ID)
		if !accepted.OwnerVerified || accepted.Status != profile.StatusActive || accepted.TaintSource != "" {
			t.Fatalf("accepted fact = %+v, want active verified with cleared taint_source", accepted)
		}
	})

	t.Run("chat preview and code confirmation", func(t *testing.T) {
		rig := newOpsRig(t)
		taintedFact, err := rig.profile.Service.ProposeAgentFact(t.Context(), &profile.AgentProposal{
			OwnerID:     principal.OwnerID,
			Category:    "preference",
			Key:         "theme",
			Value:       "solarized",
			TaintSource: taintSource,
		}, profiletest.SeededAt)
		if err != nil {
			t.Fatalf("ProposeAgentFact: %v", err)
		}
		chat, err := operation.NewChat(&operation.ChatDeps{Registry: rig.Registry, Capabilities: capabilitytest.Host(t)})
		if err != nil {
			t.Fatalf("NewChat: %v", err)
		}
		runChat := func(tokens []string, key string) command.OperationReply {
			plan := chat.Plan("profile", tokens)
			if plan.Refusal != nil {
				return command.OperationReply{Lines: plan.Refusal.Lines, Code: plan.Refusal.Code}
			}
			return chat.Run(t.Context(), &command.OperationRun{
				Caller: &command.Caller{
					PrincipalID: principal.OwnerID, Class: principal.ClassOwner,
					SessionID: operationtest.SessionID, Channel: principal.ChannelTelegram, Direct: true,
				},
				Plan:           &plan,
				IdempotencyKey: key,
			})
		}

		previewReply := runChat([]string{"accept", taintedFact.ID}, "chat-1")
		if previewReply.Code != command.ErrorCodeOK {
			t.Fatalf("chat preview = %+v", previewReply)
		}
		joined := strings.Join(previewReply.Lines, "\n")
		code := extractConfirmCode(t, joined)
		if !strings.Contains(joined, "confirm with: /profile accept "+taintedFact.ID+" --apply "+code) {
			t.Errorf("chat preview missing confirm command:\n%s", joined)
		}
		if got := rig.fact(t, taintedFact.ID); got.OwnerVerified || got.TaintSource != taintSource {
			t.Fatalf("chat preview modified fact: %+v", got)
		}

		wrongReply := runChat([]string{"accept", taintedFact.ID, "--apply", "0000-0000"}, "chat-2")
		if wrongReply.Code != command.ErrorCodeUsage || !strings.HasPrefix(wrongReply.Lines[0], "operation_confirmation_invalid") {
			t.Fatalf("chat wrong code = %+v, want operation_confirmation_invalid", wrongReply)
		}

		okReply := runChat([]string{"accept", taintedFact.ID, "--apply", code}, "chat-3")
		if okReply.Code != command.ErrorCodeOK || !strings.Contains(strings.Join(okReply.Lines, "\n"), "accepted: "+taintedFact.ID) {
			t.Fatalf("chat valid code = %+v, want accepted", okReply)
		}
		accepted := rig.fact(t, taintedFact.ID)
		if !accepted.OwnerVerified || accepted.TaintSource != "" {
			t.Fatalf("chat accepted fact = %+v, want verified with cleared taint_source", accepted)
		}
	})
}

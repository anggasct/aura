package toolbroker

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/durable"
	"github.com/anggasct/aura/internal/platform/effect"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/tools"
	"github.com/anggasct/aura/internal/platform/turn"
)

type holdingObserver struct {
	once    sync.Once
	running chan struct{}
}

func newHoldingObserver() *holdingObserver {
	return &holdingObserver{running: make(chan struct{})}
}

func (o *holdingObserver) Observe(ctx context.Context, observation *effect.Observation) {
	if observation.State != effect.StatePrepared {
		return
	}
	o.once.Do(func() { close(o.running) })
	<-ctx.Done()
}

func newDurableOperationRig(t *testing.T, hosted *hostedRuntime, options *operationOptions, stubs ...operationStub) *operationRig {
	t.Helper()
	seedSession(t, hosted.db)
	return newOperationRig(t, hosted.db, options, stubs...)
}

func (r *operationRig) callTwiceOnADurableTurn(t *testing.T, hosted *hostedRuntime, request *ToolRequest, between func()) (first, replay callOutcome, key string) {
	t.Helper()
	key = hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
		first = outcomeOf(r.broker.Execute(inTurn(ctx, inv), request))
		if between != nil {
			between()
		}
		replay = outcomeOf(r.broker.Execute(inTurn(ctx, inv), request))
		return nil
	})
	return first, replay, key
}

func TestOperationCallJournalsPolicyPreviewAuthorizationAndExecutionInOrder(t *testing.T) {
	hosted := startHostedRuntime(t)
	rig := newDurableOperationRig(t, hosted, &operationOptions{reachable: true}, previewedStub)
	request := operationRequest(previewedStub.name, "x")
	canonical, err := rig.broker.canonicalRequest(request)
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	canonical.PreviewDigest = "set-of-x"
	if err := rig.broker.digestRequest(&canonical); err != nil {
		t.Fatalf("digest request: %v", err)
	}
	withoutPreview, err := rig.broker.canonicalRequest(request)
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}

	var outcome callOutcome
	key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
		outcome = outcomeOf(rig.broker.Execute(inTurn(ctx, inv), request))
		return nil
	})
	if outcome.class != ResultOK {
		t.Fatalf("outcome = %+v, want ok", outcome)
	}
	rows := hosted.actionRows(t, key)
	want := "call-1-0/policy,call-1-0/preview,call-1-0/authorization,call-1-0/execution"
	if got := joinedKeys(rows); got != want {
		t.Fatalf("action keys = %s, want %s", got, want)
	}
	record, ok := decodeAuthorization(rows[2].result)
	if !ok || record.State != ApprovalApproved || record.RequestDigest != canonical.RequestDigest {
		t.Errorf("authorization record = %s, want the approval of the digest that binds the preview", rows[2].result)
	}
	if canonical.RequestDigest == withoutPreview.RequestDigest {
		t.Error("the preview digest did not change the request digest")
	}
	recorded, err := decodeOutcome(rows[1].result)
	if err != nil {
		t.Fatalf("preview action records %v, want a preview", err)
	}
	var preview Preview
	if err := json.Unmarshal(recorded, &preview); err != nil || preview.Digest != "set-of-x" || len(preview.Lines) != 2 {
		t.Errorf("preview action = %s, want the lines and the digest", recorded)
	}
}

func TestPreviewRunsOnlyAfterValidationAndDeterministicRefusals(t *testing.T) {
	cases := []struct {
		name    string
		stub    operationStub
		mutate  func(request *ToolRequest)
		class   ResultClass
		journal string
	}{
		{name: "arguments that fail the schema", stub: destructiveStub, mutate: func(r *ToolRequest) { r.Arguments = json.RawMessage(`{"target":1}`) }, class: ResultInvalidArgument},
		{name: "unknown arguments", stub: destructiveStub, mutate: func(r *ToolRequest) { r.Arguments = json.RawMessage(`{"target":"x","extra":true}`) }, class: ResultInvalidArgument},
		{name: "a trusted class", stub: destructiveStub, mutate: func(r *ToolRequest) { r.PrincipalClass = principal.ClassTrusted }, class: ResultToolTierDenied, journal: "call-1-0/policy"},
		{name: "a public class", stub: destructiveStub, mutate: func(r *ToolRequest) { r.PrincipalClass = principal.ClassPublic }, class: ResultToolTierDenied, journal: "call-1-0/policy"},
		{name: "a child origin", stub: destructiveStub, mutate: func(r *ToolRequest) { r.Origin = turn.OriginChild }, class: ResultToolTierDenied, journal: "call-1-0/policy"},
		{name: "a workflow origin", stub: destructiveStub, mutate: func(r *ToolRequest) { r.Origin = turn.OriginWorkflow }, class: ResultToolTierDenied, journal: "call-1-0/policy"},
		{name: "an unset origin", stub: destructiveStub, mutate: func(r *ToolRequest) { r.Origin = "" }, class: ResultToolTierDenied, journal: "call-1-0/policy"},
		{name: "a tainted trust grant", stub: trustGrantStub, mutate: func(r *ToolRequest) { r.Tainted = true }, class: ResultUntrustedInputTrustGrant, journal: "call-1-0/policy"},
		{name: "an unattended trust grant", stub: trustGrantStub, mutate: func(r *ToolRequest) { r.Unattended = true }, class: ResultOwnerTurnRequired, journal: "call-1-0/policy"},
		{name: "an unattended deletion", stub: destructiveStub, mutate: func(r *ToolRequest) { r.Unattended = true }, class: ResultOwnerTurnRequired, journal: "call-1-0/policy"},
		{name: "an unattended fresh approval change", stub: freshStub, mutate: func(r *ToolRequest) { r.Unattended = true }, class: ResultOwnerTurnRequired, journal: "call-1-0/policy"},
		{name: "an attached grant on a deletion", stub: destructiveStub, mutate: func(r *ToolRequest) { r.Approval = &approval.ApprovalGrant{GrantID: "g"} }, class: ResultToolTierDenied, journal: "call-1-0/policy"},
		{name: "an attached grant on a fresh approval change", stub: freshStub, mutate: func(r *ToolRequest) { r.Approval = &approval.ApprovalGrant{GrantID: "g"} }, class: ResultFreshApprovalRequired, journal: "call-1-0/policy"},
	}
	hosted := startHostedRuntime(t)
	rig := newDurableOperationRig(t, hosted, &operationOptions{reachable: true}, destructiveStub, trustGrantStub, freshStub)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := operationRequest(tc.stub.name, "x")
			tc.mutate(request)
			var outcome callOutcome
			key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
				outcome = outcomeOf(rig.broker.Execute(inTurn(ctx, inv), request))
				return nil
			})
			if outcome.class != tc.class {
				t.Errorf("outcome = %+v, want %s", outcome, tc.class)
			}
			if got := joinedKeys(hosted.actionRows(t, key)); got != tc.journal {
				t.Errorf("action keys = %q, want %q", got, tc.journal)
			}
		})
	}
	rig.requireUntouched(t)

	t.Run("an admitted request runs the previewer between policy and authorization", func(t *testing.T) {
		var outcome callOutcome
		key := hosted.run(t, func(ctx context.Context, inv durable.Invocation) error {
			outcome = outcomeOf(rig.broker.Execute(inTurn(ctx, inv), operationRequest(destructiveStub.name, "x")))
			return nil
		})
		if outcome.class != ResultOK {
			t.Fatalf("outcome = %+v, want ok", outcome)
		}
		if got, want := joinedKeys(hosted.actionRows(t, key)), "call-1-0/policy,call-1-0/preview,call-1-0/authorization,call-1-0/execution"; got != want {
			t.Errorf("action keys = %s, want %s", got, want)
		}
		if previews := rig.previewed.Load(); previews != 1 {
			t.Errorf("previewer runs = %d, want 1", previews)
		}
	})
}

func TestOperationRefusalsReplayAsTheSameClass(t *testing.T) {
	for _, class := range []ResultClass{ResultUntrustedInputTrustGrant, ResultOwnerTurnRequired, ResultFreshApprovalRequired} {
		if !class.stable() || !recordableClass(class) {
			t.Errorf("%s is not a stable class the journal records", class)
		}
	}
	long := strings.Repeat("a long label ", 200)
	cases := []struct {
		name      string
		stub      operationStub
		preview   Previewer
		mutate    func(request *ToolRequest)
		reachable bool
		flip      bool
		class     ResultClass
		prompts   int
		previews  int32
		handled   int32
	}{
		{name: "a change with nobody to ask", stub: changeStub, class: ResultOwnerTurnRequired, flip: true},
		{name: "a tainted change with nobody to ask", stub: changeStub, mutate: func(r *ToolRequest) { r.Tainted = true }, class: ResultUntrustedInputNoApprover, flip: true},
		{name: "a deletion with nobody to ask", stub: destructiveStub, class: ResultOwnerTurnRequired, previews: 1, flip: true},
		{name: "a fresh approval change with nobody to ask", stub: freshStub, class: ResultOwnerTurnRequired, flip: true},
		{name: "a deletion approved before the approver went away", stub: destructiveStub, reachable: true, class: ResultOK, prompts: 1, previews: 1, handled: 1, flip: true},
		{name: "a preview that does not fit the card", stub: destructiveStub, preview: func(context.Context, *ToolRequest) (Preview, error) {
			return Preview{Lines: []string{long}, Digest: "d"}, nil
		}, reachable: true, class: ResultApprovalDisplayTooLarge, previews: 1},
		{name: "a tainted trust grant", stub: trustGrantStub, mutate: func(r *ToolRequest) { r.Tainted = true }, reachable: true, class: ResultUntrustedInputTrustGrant},
		{name: "an unattended deletion", stub: destructiveStub, mutate: func(r *ToolRequest) { r.Unattended = true }, reachable: true, class: ResultOwnerTurnRequired},
		{name: "an attached grant on a fresh approval change", stub: freshStub, mutate: func(r *ToolRequest) { r.Approval = &approval.ApprovalGrant{GrantID: "g"} }, reachable: true, class: ResultFreshApprovalRequired},
		{name: "a call that is not the owner's", stub: destructiveStub, mutate: func(r *ToolRequest) { r.PrincipalClass = principal.ClassTrusted }, reachable: true, class: ResultToolTierDenied},
	}
	hosted := startHostedRuntime(t)
	seedSession(t, hosted.db)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newOperationRig(t, hosted.db, &operationOptions{reachable: tc.reachable, preview: tc.preview}, tc.stub)
			request := operationRequest(tc.stub.name, "x")
			request.IdempotencyKey = "idempotency-" + strings.ReplaceAll(tc.name, " ", "-")
			if tc.mutate != nil {
				tc.mutate(request)
			}
			first, replay, key := rig.callTwiceOnADurableTurn(t, hosted, request, func() {
				if tc.flip {
					rig.reachable.Store(!tc.reachable)
				}
			})
			if first.class != tc.class || replay.class != tc.class {
				t.Errorf("first = %+v, replay = %+v, want class %s for both", first, replay, tc.class)
			}
			if first.text != replay.text {
				t.Errorf("the replay text %q differs from the first %q", replay.text, first.text)
			}
			if prompts := rig.decider.asked(); len(prompts) != tc.prompts {
				t.Errorf("approval prompts = %d, want %d", len(prompts), tc.prompts)
			}
			if previews := rig.previewed.Load(); previews != tc.previews {
				t.Errorf("previewer runs = %d, want %d", previews, tc.previews)
			}
			if handled := rig.handled.Load(); handled != tc.handled {
				t.Errorf("handler runs = %d, want %d", handled, tc.handled)
			}
			for _, row := range hosted.actionRows(t, key) {
				if !strings.HasSuffix(row.key, "/authorization") || tc.class == ResultOK {
					continue
				}
				record, ok := decodeAuthorization(row.result)
				if !ok || record.Failure == nil || record.Failure.Class != tc.class {
					t.Errorf("authorization record = %s, want a recorded %s refusal", row.result, tc.class)
				}
			}
		})
	}
}

func TestApprovedOperationSurvivesRestartBeforeChannelsHoldTheSession(t *testing.T) {
	for _, stub := range []operationStub{changeStub, destructiveStub} {
		t.Run(stub.name, func(t *testing.T) {
			hosted := startHostedRuntime(t)
			seedSession(t, hosted.db)
			holding := newHoldingObserver()
			before := newOperationRig(t, hosted.db, &operationOptions{reachable: true, observer: holding}, stub)
			after := newOperationRig(t, hosted.db, &operationOptions{reachable: false}, stub)
			var outcomes []callOutcome
			key := hosted.start(t, func(ctx context.Context, inv durable.Invocation) error {
				rig := before
				if len(outcomes) > 0 {
					rig = after
				}
				outcomes = append(outcomes, outcomeOf(rig.broker.Execute(inTurn(ctx, inv), operationRequest(stub.name, "x"))))
				return nil
			})
			awaitClosed(t, holding.running)
			if _, err := hosted.db.ExecContext(t.Context(), `DELETE FROM durable_host`); err != nil {
				t.Fatalf("delete the host lease: %v", err)
			}
			hosted.awaitEnded(t, key, 2)

			if len(outcomes) != 2 || outcomes[1].class != ResultOK {
				t.Fatalf("outcomes = %+v, want ok after the restart", outcomes)
			}
			if prompts := before.decider.asked(); len(prompts) != 1 {
				t.Errorf("approval prompts before the restart = %d, want 1", len(prompts))
			}
			if prompts := after.decider.asked(); len(prompts) != 0 {
				t.Errorf("approval prompts after the restart = %d, want none", len(prompts))
			}
			if before.handled.Load() != 0 || after.handled.Load() != 1 {
				t.Errorf("handler runs = %d before and %d after the restart, want 0 and 1", before.handled.Load(), after.handled.Load())
			}
			if previews := after.previewed.Load(); previews != 0 {
				t.Errorf("previewer runs after the restart = %d, want the journaled preview read", previews)
			}
			requireStates(t, after.observed.list(), ApprovalApproved)
			if providers := after.intentProviders(t); len(providers) != 1 || providers[0] != operationProvider {
				t.Errorf("effect providers = %v, want one %s intent", providers, operationProvider)
			}
			succeeded, err := after.executor.Journal().ListByState(t.Context(), effect.StateSucceeded, 0)
			if err != nil || len(succeeded) != 1 {
				t.Errorf("succeeded intents = %d (%v), want exactly one application", len(succeeded), err)
			}
		})
	}
}

func TestOperationToolsStayOutOfTheLegacyEffectProvider(t *testing.T) {
	if effectProvider == operationProvider {
		t.Fatal("operations share an effect provider with the built-in tools")
	}
	definition := tools.Definition{Name: "legacy", Version: tools.Version}
	if providerFor(&definition) != effectProvider {
		t.Errorf("a definition without an operation uses %s, want %s", providerFor(&definition), effectProvider)
	}
	definition.Operation = "group.verb"
	if providerFor(&definition) != operationProvider {
		t.Errorf("a definition with an operation uses %s, want %s", providerFor(&definition), operationProvider)
	}
}

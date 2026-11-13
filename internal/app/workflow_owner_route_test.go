package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/channels/discord"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/channels/telegram"
	"github.com/anggasct/aura/internal/platform/principal"
)

type recordingOwnerApprover struct {
	prompts []approval.Prompt
	accept  bool
	err     error
}

func (r *recordingOwnerApprover) DecideForOwner(_ context.Context, prompt *approval.Prompt, _ *approval.Requester) (bool, error) {
	r.prompts = append(r.prompts, *prompt)
	return r.accept, r.err
}

func workflowRunPrompt(expiresAt time.Time, timeout time.Duration) *approval.Prompt {
	return &approval.Prompt{
		ToolName:    "write_file",
		SessionID:   "wf-run-1",
		TurnID:      "turn-1",
		PrincipalID: "telegram:7",
		Arguments:   `{"path":"a.txt"}`,
		ReasonCode:  "approval_required",
		Timeout:     timeout,
		ExpiresAt:   expiresAt,

		StandingGrantAllowed: true,
	}
}

func workflowRunRequester() *approval.Requester {
	return &approval.Requester{Principal: "telegram:7", Class: principal.ClassTrusted, Persona: "lobby"}
}

func TestWorkflowOwnerRouteHoldsOnlyInFlightCalls(t *testing.T) {
	log := &askLog{}
	route, err := newWorkflowRoute([]approvalAdapter{&scriptedApprover{name: "telegram", accept: true, log: log}}, nil)
	if err != nil || route == nil {
		t.Fatalf("newWorkflowRoute(adapter) = %v, %v, want a route", route, err)
	}
	const session = "wf-run-1"
	if route.HoldsApprovalTarget(session) {
		t.Fatal("session held before Hold")
	}
	releaseFirst := route.Hold(session, workflowRunRequester())
	releaseSecond := route.Hold(session, workflowRunRequester())
	if !route.HoldsApprovalTarget(session) {
		t.Fatal("session not held between Hold and its release")
	}
	releaseFirst()
	if !route.HoldsApprovalTarget(session) {
		t.Fatal("session released while a nested hold is still open")
	}
	releaseSecond()
	if route.HoldsApprovalTarget(session) {
		t.Fatal("session still held after every release")
	}

	t.Run("a prompt of another principal is refused", func(t *testing.T) {
		release := route.Hold(session, workflowRunRequester())
		t.Cleanup(release)
		prompt := workflowRunPrompt(time.Now().Add(time.Hour), time.Minute)
		prompt.PrincipalID = "telegram:8"
		accepted, err := route.Decide(t.Context(), prompt)
		if err != nil || accepted {
			t.Fatalf("Decide(other principal) = %v, %v, want a refusal", accepted, err)
		}
		if asked := log.approvers(); len(asked) != 0 {
			t.Fatalf("owner approver asked %v, want nobody", asked)
		}
	})
	t.Run("a prompt of the held origin reaches the owner", func(t *testing.T) {
		release := route.Hold(session, workflowRunRequester())
		t.Cleanup(release)
		accepted, err := route.Decide(t.Context(), workflowRunPrompt(time.Now().Add(time.Hour), time.Minute))
		if err != nil || !accepted {
			t.Fatalf("Decide(held origin) = %v, %v, want an approval", accepted, err)
		}
		if asked := log.approvers(); len(asked) != 1 {
			t.Fatalf("owner approver asked %v, want one card", asked)
		}
	})
}

func TestWorkflowOwnerApproverStripsTheStandingOffer(t *testing.T) {
	owners := &recordingOwnerApprover{accept: true}
	approver := &workflowOwnerApprover{owners: owners, now: time.Now}
	prompt := workflowRunPrompt(time.Now().Add(time.Hour), time.Minute)
	accepted, err := approver.DecideForOwner(t.Context(), prompt, workflowRunRequester())
	if err != nil || !accepted {
		t.Fatalf("DecideForOwner() = %v, %v, want an approval", accepted, err)
	}
	if len(owners.prompts) != 1 || owners.prompts[0].StandingGrantAllowed {
		t.Fatalf("owner approver prompts = %+v, want one without the standing offer", owners.prompts)
	}
	if !prompt.StandingGrantAllowed {
		t.Fatal("the caller's prompt was changed")
	}
}

func TestWorkflowOwnerApproverLeavesTimeToRun(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	const execution = 30 * time.Second
	cases := []struct {
		name      string
		expiresAt time.Time
		wantCard  bool
		wantAt    time.Time
	}{
		{name: "the card closes before the execution budget starts", expiresAt: now.Add(10 * time.Minute), wantCard: true, wantAt: now.Add(10*time.Minute - execution)},
		{name: "no time left opens no card", expiresAt: now.Add(execution), wantCard: false},
		{name: "less than the execution budget opens no card", expiresAt: now.Add(execution / 2), wantCard: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owners := &recordingOwnerApprover{accept: true}
			approver := &workflowOwnerApprover{owners: owners, now: func() time.Time { return now }}
			accepted, err := approver.DecideForOwner(t.Context(), workflowRunPrompt(tc.expiresAt, execution), workflowRunRequester())
			if err != nil {
				t.Fatalf("DecideForOwner() error = %v", err)
			}
			if accepted != tc.wantCard {
				t.Fatalf("accepted = %v, want %v", accepted, tc.wantCard)
			}
			if !tc.wantCard {
				if len(owners.prompts) != 0 {
					t.Fatalf("owner approver asked %d times, want a refusal with no card", len(owners.prompts))
				}
				return
			}
			if len(owners.prompts) != 1 || !owners.prompts[0].ExpiresAt.Equal(tc.wantAt) {
				t.Fatalf("owner approver prompts = %+v, want one expiring at %v", owners.prompts, tc.wantAt)
			}
		})
	}
}

func TestWorkflowOwnerApproverTurnsAnUnansweredCardIntoARefusal(t *testing.T) {
	failure := errors.New("owner dm exploded")
	cases := []struct {
		name    string
		err     error
		wantErr error
	}{
		{name: "discord unavailable", err: discordUnavailable()},
		{name: "telegram unavailable", err: telegramUnavailable()},
		{name: "discord timeout", err: discord.Errorf(discord.ErrorCodeApprovalTimeout, "owner did not answer the approval in time")},
		{name: "telegram timeout", err: telegram.Errorf(telegram.ErrorCodeApprovalTimeout, "owner did not answer the approval in time")},
		{name: "context cancelled", err: context.Canceled, wantErr: context.Canceled},
		{name: "deadline exceeded", err: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
		{name: "uncoded failure", err: failure, wantErr: failure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &scriptedApprover{name: "owner", err: tc.err, log: &askLog{}}
			approver := &workflowOwnerApprover{owners: newOwnerApprovalRouter([]approvalAdapter{adapter}, nil), now: time.Now}
			accepted, err := approver.DecideForOwner(t.Context(), workflowRunPrompt(time.Now().Add(time.Hour), time.Minute), workflowRunRequester())
			if accepted {
				t.Fatal("an unanswered card was approved")
			}
			if tc.wantErr == nil && err != nil {
				t.Fatalf("DecideForOwner() error = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("DecideForOwner() error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

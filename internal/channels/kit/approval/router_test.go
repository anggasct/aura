package approval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/principal"
)

type routerAskLog struct {
	mu    sync.Mutex
	asked []string
}

func (l *routerAskLog) record(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, name)
}

func (l *routerAskLog) approvers() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.asked)
}

type scriptedRouterApprover struct {
	name   string
	serves string
	accept bool
	err    error
	log    *routerAskLog
}

func (s *scriptedRouterApprover) ServesPersona(persona string) bool {
	return s.serves != "" && s.serves == persona
}

func (s *scriptedRouterApprover) DecideForOwner(_ context.Context, _ *Prompt, _ *Requester) (bool, error) {
	s.log.record(s.name)
	return s.accept, s.err
}

type routerResults struct {
	mu      sync.Mutex
	results []string
}

func (r *routerResults) observe(_ context.Context, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, result)
}

func (r *routerResults) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.results)
}

func routerPrompt() *Prompt {
	return &Prompt{ToolName: "write_file", SessionID: "lobby/http/key/session", PrincipalID: "http:key", ExpiresAt: time.Now().Add(time.Minute)}
}

func routerRequester() *Requester {
	return &Requester{Principal: "http:key", Class: principal.ClassPublic, Persona: "lobby"}
}

func TestOwnerApprovalRouterOrdersAndFallsThrough(t *testing.T) {
	discordUnavailable := fmt.Errorf("owner has no discord identity: %w", ErrUnavailable)
	telegramUnavailable := fmt.Errorf("owner has no telegram identity: %w", ErrUnavailable)
	timeout := fmt.Errorf("owner did not answer the approval in time: %w", ErrTimeout)
	failure := errors.New("owner dm exploded")
	cases := []struct {
		name         string
		build        func(log *routerAskLog) []PersonaOwnerApprover
		wantAccepted bool
		wantErr      func(error) bool
		wantAsked    []string
	}{
		{
			name: "persona serving approver goes first",
			build: func(log *routerAskLog) []PersonaOwnerApprover {
				return []PersonaOwnerApprover{
					&scriptedRouterApprover{name: "discord", accept: false, log: log},
					&scriptedRouterApprover{name: "telegram", serves: "lobby", accept: true, log: log},
				}
			},
			wantAccepted: true,
			wantAsked:    []string{"telegram"},
		},
		{
			name: "config order holds among approvers that serve the persona",
			build: func(log *routerAskLog) []PersonaOwnerApprover {
				return []PersonaOwnerApprover{
					&scriptedRouterApprover{name: "discord", serves: "lobby", accept: true, log: log},
					&scriptedRouterApprover{name: "telegram", serves: "lobby", accept: false, log: log},
				}
			},
			wantAccepted: true,
			wantAsked:    []string{"discord"},
		},
		{
			name: "config order holds when none serves the persona",
			build: func(log *routerAskLog) []PersonaOwnerApprover {
				return []PersonaOwnerApprover{
					&scriptedRouterApprover{name: "discord", err: discordUnavailable, log: log},
					&scriptedRouterApprover{name: "telegram", accept: true, log: log},
				}
			},
			wantAccepted: true,
			wantAsked:    []string{"discord", "telegram"},
		},
		{
			name: "unavailable serving approver falls through to the rest",
			build: func(log *routerAskLog) []PersonaOwnerApprover {
				return []PersonaOwnerApprover{
					&scriptedRouterApprover{name: "discord", accept: true, log: log},
					&scriptedRouterApprover{name: "telegram", serves: "lobby", err: telegramUnavailable, log: log},
				}
			},
			wantAccepted: true,
			wantAsked:    []string{"telegram", "discord"},
		},
		{
			name: "a rejection is final",
			build: func(log *routerAskLog) []PersonaOwnerApprover {
				return []PersonaOwnerApprover{
					&scriptedRouterApprover{name: "discord", accept: false, log: log},
					&scriptedRouterApprover{name: "telegram", accept: true, log: log},
				}
			},
			wantAsked: []string{"discord"},
		},
		{
			name: "a timeout is final",
			build: func(log *routerAskLog) []PersonaOwnerApprover {
				return []PersonaOwnerApprover{
					&scriptedRouterApprover{name: "discord", err: timeout, log: log},
					&scriptedRouterApprover{name: "telegram", accept: true, log: log},
				}
			},
			wantErr:   func(err error) bool { return errors.Is(err, ErrTimeout) },
			wantAsked: []string{"discord"},
		},
		{
			name: "an uncoded failure is final",
			build: func(log *routerAskLog) []PersonaOwnerApprover {
				return []PersonaOwnerApprover{
					&scriptedRouterApprover{name: "discord", err: failure, log: log},
					&scriptedRouterApprover{name: "telegram", accept: true, log: log},
				}
			},
			wantErr:   func(err error) bool { return errors.Is(err, failure) },
			wantAsked: []string{"discord"},
		},
		{
			name: "every approver unavailable denies",
			build: func(log *routerAskLog) []PersonaOwnerApprover {
				return []PersonaOwnerApprover{
					&scriptedRouterApprover{name: "discord", err: discordUnavailable, log: log},
					&scriptedRouterApprover{name: "telegram", err: telegramUnavailable, log: log},
				}
			},
			wantErr:   func(err error) bool { return errors.Is(err, ErrUnavailable) },
			wantAsked: []string{"discord", "telegram"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &routerAskLog{}
			routed := &routerResults{}
			router := NewOwnerRouter(tc.build(log), routed.observe)
			accepted, err := router.DecideForOwner(t.Context(), routerPrompt(), routerRequester())
			if accepted != tc.wantAccepted {
				t.Errorf("accepted = %v, want %v", accepted, tc.wantAccepted)
			}
			if (tc.wantErr == nil) != (err == nil) || (tc.wantErr != nil && !tc.wantErr(err)) {
				t.Errorf("err = %v", err)
			}
			if got := log.approvers(); !slices.Equal(got, tc.wantAsked) {
				t.Errorf("asked = %v, want %v", got, tc.wantAsked)
			}
			if got := routed.recorded(); len(got) != 0 {
				t.Errorf("router recorded %v; adapters own their observations", got)
			}
		})
	}
}

func TestOwnerApprovalRouterWithoutApproversDeniesAndRecordsUnavailable(t *testing.T) {
	routed := &routerResults{}
	router := NewOwnerRouter(nil, routed.observe)
	accepted, err := router.DecideForOwner(t.Context(), routerPrompt(), routerRequester())
	if accepted || err != nil {
		t.Errorf("DecideForOwner() = %v, %v; want a plain denial", accepted, err)
	}
	if got := routed.recorded(); !slices.Equal(got, []string{RoutedUnavailable}) {
		t.Errorf("routed observations = %v, want one unavailable", got)
	}
}

func TestOwnerRouterAndWorkflowApproverRejectNilArguments(t *testing.T) {
	var nilRouter *OwnerRouter
	emptyRouter := NewOwnerRouter(nil, nil)
	populatedRouter := NewOwnerRouter([]PersonaOwnerApprover{
		&scriptedRouterApprover{name: "discord", accept: true, log: &routerAskLog{}},
	}, nil)
	var nilWorkflow *WorkflowApprover
	emptyWorkflow := NewWorkflowApprover(nil, time.Now)
	populatedWorkflow := NewWorkflowApprover(populatedRouter, time.Now)

	cases := []struct {
		name string
		call func() (bool, error)
	}{
		{"nil router", func() (bool, error) { return nilRouter.DecideForOwner(t.Context(), routerPrompt(), routerRequester()) }},
		{"empty router nil prompt", func() (bool, error) { return emptyRouter.DecideForOwner(t.Context(), nil, routerRequester()) }},
		{"empty router nil requester", func() (bool, error) { return emptyRouter.DecideForOwner(t.Context(), routerPrompt(), nil) }},
		{"populated router nil prompt", func() (bool, error) { return populatedRouter.DecideForOwner(t.Context(), nil, routerRequester()) }},
		{"populated router nil requester", func() (bool, error) { return populatedRouter.DecideForOwner(t.Context(), routerPrompt(), nil) }},
		{"nil workflow approver", func() (bool, error) {
			return nilWorkflow.DecideForOwner(t.Context(), routerPrompt(), routerRequester())
		}},
		{"workflow approver nil owners", func() (bool, error) {
			return emptyWorkflow.DecideForOwner(t.Context(), routerPrompt(), routerRequester())
		}},
		{"workflow approver nil prompt", func() (bool, error) {
			return populatedWorkflow.DecideForOwner(t.Context(), nil, routerRequester())
		}},
		{"workflow approver nil requester", func() (bool, error) {
			return populatedWorkflow.DecideForOwner(t.Context(), routerPrompt(), nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accepted, err := tc.call()
			if accepted || !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Errorf("DecideForOwner() = %v, %v; want false with invalid_argument", accepted, err)
			}
		})
	}
}

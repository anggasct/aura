package handoff

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

var ErrNoTarget = errors.New("handoff: no target")

type Target struct {
	Channel   string
	Account   string
	ChannelID string
	ThreadID  string
}

type Notifier interface {
	NotifyHandoff(ctx context.Context, sessionID, reason string) (Target, error)
}

type Service struct {
	handoffs  Store
	notifiers []Notifier
	now       func() time.Time
}

func NewService(handoffs Store, notifiers []Notifier, now func() time.Time) (*Service, error) {
	if handoffs == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "handoff store must not be nil")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{handoffs: handoffs, notifiers: slices.Clone(notifiers), now: now}, nil
}

func (s *Service) OpenHandoff(ctx context.Context, request *Request) error {
	if request == nil {
		return fault.New(ErrorCodeInvalidArgument, "handoff request must not be nil")
	}
	for _, notifier := range s.notifiers {
		target, err := notifier.NotifyHandoff(ctx, request.SessionID, request.Reason)
		if errors.Is(err, ErrNoTarget) {
			continue
		}
		if err != nil {
			return err
		}
		return s.handoffs.Open(ctx, &Handoff{
			SessionID:      request.SessionID,
			Reason:         request.Reason,
			PrincipalID:    request.PrincipalID,
			AgentID:        request.AgentID,
			Account:        target.Account,
			Channel:        target.Channel,
			ReplyChannelID: target.ChannelID,
			ReplyThreadID:  target.ThreadID,
			OpenedAt:       s.now().UTC(),
		})
	}
	return fault.New(ErrorCodeUnavailable, "handoff: no channel knows where this session's requester is")
}

type Gate struct {
	handoffs Store
	timeout  time.Duration
	now      func() time.Time
}

func NewGate(handoffs Store, timeout time.Duration, now func() time.Time) (*Gate, error) {
	if handoffs == nil {
		return nil, fault.New(ErrorCodeInvalidArgument, "handoff store must not be nil")
	}
	if now == nil {
		now = time.Now
	}
	return &Gate{handoffs: handoffs, timeout: timeout, now: now}, nil
}

func (g *Gate) Check(ctx context.Context, sessionID string) (string, error) {
	state, _, err := g.handoffs.Check(ctx, sessionID, g.timeout, g.now().UTC())
	if err != nil {
		return StateNone, err
	}
	switch state {
	case StateOpen:
		return StateOpen, nil
	case StateExpired:
		return StateExpired, nil
	default:
		return StateNone, nil
	}
}

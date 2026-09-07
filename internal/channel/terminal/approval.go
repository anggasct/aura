package terminal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

func errNilArgument(name string) error {
	return fmt.Errorf("invalid_argument: %s must not be nil", name)
}

type ApprovalCard struct {
	ToolName       string
	ToolVersion    string
	SessionID      string
	TurnID         string
	PrincipalID    string
	Arguments      string
	Network        bool
	Timeout        time.Duration
	MaxOutputBytes int64
	PolicyVersion  string
	ReasonCode     string
	ExpiresAt      time.Time
}

type approvalAsk struct {
	card    *ApprovalCard
	reply   chan bool
	claimed bool
}

func (a *approvalAsk) answer(accepted bool) {
	select {
	case a.reply <- accepted:
	default:
	}
}

type ApprovalBridge struct {
	mu      sync.Mutex
	pending *approvalAsk
	ready   chan struct{}
}

func NewApprovalBridge() *ApprovalBridge {
	return &ApprovalBridge{ready: make(chan struct{}, 1)}
}

func (b *ApprovalBridge) Decide(ctx context.Context, card *ApprovalCard) (bool, error) {
	if ctx == nil {
		return false, fmt.Errorf("terminal: %w", errNilArgument("context"))
	}
	if card == nil {
		return false, fmt.Errorf("terminal: %w", errNilArgument("card"))
	}
	ask := &approvalAsk{card: card, reply: make(chan bool, 1)}
	b.mu.Lock()
	if b.pending != nil {
		b.mu.Unlock()
		return false, errors.New("terminal: another approval is already pending")
	}
	b.pending = ask
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.pending == ask {
			b.pending = nil
		}
		b.mu.Unlock()
	}()
	select {
	case b.ready <- struct{}{}:
	default:
	}
	select {
	case accepted := <-ask.reply:
		return accepted, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (b *ApprovalBridge) readyCh() <-chan struct{} {
	return b.ready
}

func (b *ApprovalBridge) take() *approvalAsk {
	b.mu.Lock()
	defer b.mu.Unlock()
	ask := b.pending
	if ask == nil || ask.claimed {
		return nil
	}
	ask.claimed = true
	return ask
}

func approvalAccepted(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

func renderApprovalCard(card *ApprovalCard, width int) string {
	arguments := limitText(sanitizeText(card.Arguments))
	if arguments == "" {
		arguments = "{}"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "approval required: %s@%s\n", sanitizeText(card.ToolName), sanitizeText(card.ToolVersion))
	fmt.Fprintf(&b, "  session: %s\n", sanitizeText(card.SessionID))
	fmt.Fprintf(&b, "  principal: %s\n", sanitizeText(card.PrincipalID))
	fmt.Fprintf(&b, "  arguments: %s\n", arguments)
	fmt.Fprintf(&b, "  effect: network=%t timeout=%s max_output_bytes=%d\n", card.Network, card.Timeout, card.MaxOutputBytes)
	fmt.Fprintf(&b, "  policy: %s (%s)\n", sanitizeText(card.PolicyVersion), sanitizeText(card.ReasonCode))
	if !card.ExpiresAt.IsZero() {
		fmt.Fprintf(&b, "  expires: %s\n", card.ExpiresAt.UTC().Format(time.RFC3339))
	}
	b.WriteString("approve exactly this request? [y/N] ")
	cardText := b.String()
	if width < 1 {
		return cardText
	}
	physical := truncateLines(wrapText(cardText, width), 64)
	lines := make([]string, len(physical))
	for i, line := range physical {
		lines[i] = line.text
	}
	return strings.Join(lines, "\n")
}

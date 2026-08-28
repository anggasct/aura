package terminal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ApprovalCard is the display-safe canonical scope of one exact approval
// request. Values are sanitized before they reach the output surface and
// secret material is already redacted upstream.
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

// approvalAsk is one pending card plus its reply path.
type approvalAsk struct {
	card  *ApprovalCard
	reply chan bool
}

func (a *approvalAsk) answer(accepted bool) {
	select {
	case a.reply <- accepted:
	default:
	}
}

// ApprovalBridge connects the runtime's approval decisions to the console's
// input loop. The turn worker asks through Decide and blocks; the console
// renders the card, routes the operator's answer, and replies. Anything that
// ends the ask without an explicit acceptance — cancellation, EOF, expiry,
// turn end, a second concurrent ask — rejects.
type ApprovalBridge struct {
	mu      sync.Mutex
	pending *approvalAsk
	ready   chan struct{}
}

// NewApprovalBridge builds an idle bridge.
func NewApprovalBridge() *ApprovalBridge {
	return &ApprovalBridge{ready: make(chan struct{}, 1)}
}

// Decide blocks the caller — the runtime turn worker — until the console
// returns the operator's answer or ctx ends.
func (b *ApprovalBridge) Decide(ctx context.Context, card *ApprovalCard) (bool, error) {
	if ctx == nil {
		return false, errors.New("terminal: approval context must not be nil")
	}
	if card == nil {
		return false, errors.New("terminal: approval card must not be nil")
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

// readyCh exposes the ask signal; it closes never and fires once per ask.
func (b *ApprovalBridge) readyCh() <-chan struct{} {
	return b.ready
}

// take claims the pending ask for rendering; nil when none is pending.
func (b *ApprovalBridge) take() *approvalAsk {
	b.mu.Lock()
	defer b.mu.Unlock()
	ask := b.pending
	if ask != nil {
		b.pending = nil
	}
	return ask
}

// approvalAccepted reports whether one input line is an explicit approval.
// Everything else — including the empty default — rejects.
func approvalAccepted(line string) bool {
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// renderApprovalCard lays out the bounded canonical scope. Every value is
// sanitized and the card wraps to the display width so nothing escapes the
// approval surface.
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

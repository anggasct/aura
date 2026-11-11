package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"time"

	"github.com/anggasct/aura/internal/channels/terminal"
	"github.com/anggasct/aura/internal/platform/approval"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/telemetry"
	"github.com/anggasct/aura/internal/platform/toolbroker"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"
)

type terminalBroker struct{}

func (terminalBroker) Evaluate(ctx context.Context, request *approval.ToolRequest) (approval.PolicyDecision, error) {
	return approval.PolicyDecision{Outcome: approval.OutcomeDeny, ReasonCode: "plain_console_denies_tools"}, nil
}

type terminalRunner struct {
	engine  turn.AgentRuntime
	agentID string
}

func (r *terminalRunner) Run(ctx context.Context, req *terminal.Request) iter.Seq2[terminal.Event, error] {
	return func(yield func(terminal.Event, error) bool) {
		if req == nil {
			yield(terminal.Event{}, errors.New("terminal: request must not be nil"))
			return
		}
		parts := make([]runtimeingress.InputPart, len(req.Parts))
		for i := range req.Parts {
			parts[i] = runtimeingress.InputPart{Text: req.Parts[i].Text, Image: req.Parts[i].Image}
		}
		agentID := req.AgentID
		if agentID == "" {
			agentID = r.agentID
		}
		runtimeReq := &turn.TurnRequest{
			SessionID:      req.SessionID,
			PrincipalID:    req.PrincipalID,
			PrincipalClass: principal.ClassOwner,
			Origin:         turn.OriginTerminal,
			Parts:          parts,
			IdempotencyKey: req.IdempotencyKey,
			AgentID:        agentID,
			Skill:          req.Skill,
		}
		for ev, err := range r.engine.Run(ctx, runtimeReq) {
			if err != nil {
				yield(terminal.Event{}, err)
				return
			}
			if !yield(terminal.Event{
				Kind:    ev.Kind,
				Author:  ev.Author,
				TurnID:  ev.TurnID,
				Payload: ev.Payload,
			}, nil) {
				return
			}
		}
	}
}

type terminalSessions struct {
	sessions store.SessionService
	newID    func() (string, error)
	agentID  string
}

var _ terminal.Sessions = (*terminalSessions)(nil)

func (s *terminalSessions) Create(ctx context.Context, owner, agentID string) (terminal.Session, error) {
	newID := s.newID
	if newID == nil {
		newID = newTerminalSessionID
	}
	id, err := newID()
	if err != nil {
		return terminal.Session{}, fmt.Errorf("terminal: create session id: %w", err)
	}
	if agentID == "" {
		agentID = s.agentID
	}
	internal := &store.Session{
		ID:        id,
		OwnerID:   owner,
		AgentID:   agentID,
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if err := s.sessions.Create(ctx, internal); err != nil {
		return terminal.Session{}, err
	}
	return terminal.Session{ID: internal.ID, OwnerID: internal.OwnerID, AgentID: internal.AgentID}, nil
}

func (s *terminalSessions) Get(ctx context.Context, id string) (terminal.Session, error) {
	internal, err := s.sessions.Get(ctx, id)
	if err != nil {
		if code, ok := store.CodeOf(err); errors.Is(err, sql.ErrNoRows) || (ok && code == store.ErrorCodeSessionNotFound) {
			return terminal.Session{}, fmt.Errorf("%w: %s", terminal.ErrSessionNotFound, id)
		}
		return terminal.Session{}, err
	}
	return terminal.Session{ID: internal.ID, OwnerID: internal.OwnerID, AgentID: internal.AgentID}, nil
}

func (s *terminalSessions) ListEvents(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]terminal.Event, error) {
	events, err := s.sessions.ListEvents(ctx, sessionID, afterSequence, limit)
	if err != nil {
		return nil, err
	}
	out := make([]terminal.Event, len(events))
	for i := range events {
		out[i] = terminal.Event{
			Kind:    events[i].Kind,
			Author:  events[i].Author,
			TurnID:  events[i].TurnID,
			Payload: events[i].Payload,
		}
	}
	return out, nil
}

type chatPresentation struct {
	plain   bool
	noColor bool
}

func shouldUseTTY(present chatPresentation, inTTY, outTTY bool) bool {
	if present.plain {
		return false
	}
	return inTTY && outTTY
}

type chatBuild struct {
	inFile    *os.File
	outFile   *os.File
	useTTY    bool
	approvals *terminal.ApprovalBridge
	agentID   string
}

func (b *build) chatToolWiring(ctx context.Context) (toolWiring, error) {
	chat, opts := b.chat, &b.opts.Chat
	inFile, inOk := opts.In.(*os.File)
	outFile, outOk := opts.Out.(*os.File)
	if inOk && outOk {
		chat.inFile, chat.outFile = inFile, outFile
		chat.useTTY = shouldUseTTY(chatPresentation{plain: opts.Plain, noColor: opts.NoColor}, terminal.IsTerminal(int(inFile.Fd())), terminal.IsTerminal(int(outFile.Fd())))
	}
	agentID, err := resolveChatPersona(ctx, b.registry, b.sessions, opts.SessionID, opts.Persona)
	if err != nil {
		return toolWiring{}, err
	}
	chat.agentID = agentID
	if b.cfg.Tools != nil && chat.useTTY {
		chat.approvals = terminal.NewApprovalBridge()
	}
	return toolWiring{
		decider:   approvalDeciderFor(chat.approvals),
		reachable: terminalApprovalReachable(chat.approvals),
	}, nil
}

func (b *build) buildChatSurface() error {
	cfg, chat, opts := b.cfg, b.chat, &b.opts.Chat
	console := terminal.NewConsole(
		&terminalRunner{engine: b.engine, agentID: chat.agentID},
		&terminalSessions{sessions: b.sessions, agentID: chat.agentID},
		terminal.PlainRenderer{},
		opts.In, opts.Out, opts.Diag,
		terminal.Config{
			MaxInputBytes:       cfg.Terminal.MaxInputBytes,
			InMemoryHistory:     cfg.Terminal.InMemoryHistory,
			SecondInterruptTime: time.Duration(cfg.Terminal.SecondInterruptTime),
		},
		principal.OwnerID,
	)
	commandRecorder, err := telemetry.NewCommandRecorder(nil, nil)
	if err != nil {
		return err
	}
	console.SetCommandRecorder(commandRecorder)
	if b.modules.skills != nil {
		console.SetSkills(b.modules.skills)
	}
	if closer, ok := opts.In.(io.Closer); ok {
		console.SetInputCloser(func() { _ = closer.Close() })
	}
	console.SetSessionID(opts.SessionID)
	console.SetAgentID(chat.agentID)
	if chat.useTTY {
		outFile := chat.outFile
		outFD := int(outFile.Fd())
		console.SetTTY(terminal.NewTTYRenderer(terminal.TTYOptions{
			Out:     terminal.NewTTYOutput(outFile),
			Width:   func() int { w, _ := terminal.TerminalSize(outFD); return w },
			Hz:      cfg.Terminal.RenderHz,
			Styling: !opts.NoColor,
			Stdin:   chat.inFile,
			Stdout:  outFile,
		}))
		console.SetApprovalBridge(chat.approvals)
	}
	b.serve = serveConsole(console, opts.Interrupts)
	return nil
}

func serveConsole(console *terminal.Console, signals <-chan os.Signal) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		interrupts, stopInterrupts := forwardInterrupts(ctx, signals)
		defer stopInterrupts()
		console.SetInterrupts(interrupts)
		return console.Run(ctx)
	}
}

func terminalApprovalReachable(approvals *terminal.ApprovalBridge) func(sessionID string) bool {
	if approvals == nil {
		return nil
	}
	return func(string) bool { return true }
}

func approvalDeciderFor(approvals *terminal.ApprovalBridge) toolbroker.ApprovalDecider {
	if approvals == nil {
		return nil
	}
	return func(ctx context.Context, prompt *toolbroker.ApprovalPrompt) (bool, error) {
		if prompt == nil {
			return false, errors.New("approval prompt must not be nil")
		}
		return approvals.Decide(ctx, &terminal.ApprovalCard{
			ToolName:       prompt.ToolName,
			ToolVersion:    prompt.ToolVersion,
			SessionID:      prompt.SessionID,
			TurnID:         prompt.TurnID,
			PrincipalID:    prompt.PrincipalID,
			Arguments:      prompt.Arguments,
			Network:        prompt.Network,
			Timeout:        prompt.Timeout,
			MaxOutputBytes: prompt.MaxOutputBytes,
			PolicyVersion:  prompt.PolicyVersion,
			ReasonCode:     prompt.ReasonCode,
			ExpiresAt:      prompt.ExpiresAt,
		})
	}
}

func newTerminalSessionID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "sess_" + hex.EncodeToString(b[:]), nil
}

type interruptContextKey struct{}

func WithInterrupts(ctx context.Context, signals <-chan os.Signal) context.Context {
	return context.WithValue(ctx, interruptContextKey{}, signals)
}

func interruptsFromContext(ctx context.Context) <-chan os.Signal {
	signals, _ := ctx.Value(interruptContextKey{}).(<-chan os.Signal)
	return signals
}

func forwardInterrupts(ctx context.Context, signals <-chan os.Signal) (events <-chan struct{}, stop func()) {
	if signals == nil {
		return nil, func() {}
	}
	forwardCtx, cancel := context.WithCancel(ctx)
	out := make(chan struct{}, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(out)
		for {
			select {
			case <-forwardCtx.Done():
				return
			case _, ok := <-signals:
				if !ok {
					return
				}
				select {
				case out <- struct{}{}:
				case <-forwardCtx.Done():
					return
				}
			}
		}
	}()
	stop = func() {
		cancel()
		<-done
	}
	return out, stop
}

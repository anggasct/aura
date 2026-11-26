package app

import (
	"context"
	"io"
	"os"

	"github.com/anggasct/aura/internal/channels/terminal"
	"github.com/anggasct/aura/internal/platform/command"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/telemetry"
)

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
		decider:   terminal.ApprovalDecider(chat.approvals),
		reachable: terminal.ApprovalReachable(chat.approvals),
	}, nil
}

func (b *build) buildChatSurface() error {
	cfg, chat, opts := b.cfg, b.chat, &b.opts.Chat
	runner, err := terminal.NewRuntimeRunner(b.engine, chat.agentID)
	if err != nil {
		return err
	}
	sessions, err := terminal.NewTurnSessions(b.sessions, chat.agentID)
	if err != nil {
		return err
	}
	commandRecorder, err := telemetry.NewCommandRecorder(nil, nil)
	if err != nil {
		return err
	}
	var skills command.Skills
	if b.modules.skills != nil {
		skills = b.modules.skills
	}
	var closeInput func()
	if closer, ok := opts.In.(io.Closer); ok {
		closeInput = func() { _ = closer.Close() }
	}
	var tty *terminal.TTYRenderer
	var approvals *terminal.ApprovalBridge
	if chat.useTTY {
		outFile := chat.outFile
		outFD := int(outFile.Fd())
		tty = terminal.NewTTYRenderer(terminal.TTYOptions{
			Out:     terminal.NewTTYOutput(outFile),
			Width:   func() int { w, _ := terminal.TerminalSize(outFD); return w },
			Hz:      cfg.Terminal.RenderHz,
			Styling: !opts.NoColor,
			Stdin:   chat.inFile,
			Stdout:  outFile,
		})
		approvals = chat.approvals
	}
	console, err := terminal.New(&terminal.Deps{
		Runner:          runner,
		Sessions:        sessions,
		Renderer:        terminal.PlainRenderer{},
		In:              opts.In,
		Out:             opts.Out,
		Diag:            opts.Diag,
		Config:          cfg.Terminal,
		Principal:       principal.OwnerID,
		SessionID:       opts.SessionID,
		AgentID:         chat.agentID,
		Skills:          skills,
		CommandRecorder: commandRecorder,
		CloseInput:      closeInput,
		TTY:             tty,
		Approvals:       approvals,
	})
	if err != nil {
		return err
	}
	b.serve = serveConsole(console, opts.Interrupts)
	return nil
}

func serveConsole(console *terminal.Console, signals <-chan os.Signal) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		interrupts, stopInterrupts := forwardInterrupts(ctx, signals)
		defer stopInterrupts()
		return console.RunWithInterrupts(ctx, interrupts)
	}
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

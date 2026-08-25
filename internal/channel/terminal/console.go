package terminal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// ErrInterrupted marks a console run ended by an interrupt escalation.
var ErrInterrupted = errors.New("terminal: interrupted")

// Console is the plain, line-oriented local channel. It reads UTF-8
// newline-delimited prompts until EOF, submits each as a turn, and writes the
// completed assistant text to stdout while diagnostics go to stderr. It emits
// no ANSI, never presents an interactive approval, and denies approvals by
// default.
type Console struct {
	runner   Runner
	sessions Sessions
	render   Renderer

	in     io.Reader
	out    io.Writer
	diag   io.Writer
	config Config

	principal string
	sessionID string

	mu           sync.Mutex
	interrupts   <-chan struct{}
	escalated    chan struct{}
	now          func() time.Time
	lastCancelAt time.Time
	turnCancel   context.CancelFunc
}

// NewConsole builds a plain console. in/out/diag are owned by the caller; the
// console never closes them. principal is the local owner identity stamped on
// every turn and validated on session switches.
func NewConsole(runner Runner, sessions Sessions, render Renderer, in io.Reader, out, diag io.Writer, config Config, principal string) *Console {
	return &Console{
		runner:    runner,
		sessions:  sessions,
		render:    render,
		in:        in,
		out:       out,
		diag:      diag,
		config:    config,
		principal: principal,
		now:       time.Now,
	}
}

// SetClock overrides the interrupt-window clock for tests.
func (c *Console) SetClock(now func() time.Time) { c.now = now }

// SetInterrupts wires an interrupt source so tests can drive Ctrl-C without
// real signals. The channel carries one event per interrupt; nil (the
// default) means interrupts never arrive.
func (c *Console) SetInterrupts(ch <-chan struct{}) { c.interrupts = ch }

// Run drives the console until EOF after drain, an escalated interrupt, or
// ctx cancellation. A first interrupt cancels the active turn and waits for
// its durable terminal state; a second interrupt within the configured window
// escalates to Interrupted.
func (c *Console) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("terminal: context must not be nil")
	}
	if c.sessions == nil {
		return errors.New("terminal: session service must not be nil")
	}
	sess, err := c.sessions.Create(ctx, c.principal)
	if err != nil {
		if !isSessionConflict(err) {
			return fmt.Errorf("terminal: open session: %w", err)
		}
		sess, err = c.sessions.Get(ctx, c.sessionID)
		if err != nil {
			return fmt.Errorf("terminal: open session: %w", err)
		}
	}
	c.sessionID = sess.ID

	c.escalated = make(chan struct{})
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	if c.interrupts != nil {
		go c.watchInterrupts(watchCtx)
	}

	lines := readLines(ctx, c.in, c.config.MaxInputBytes)
	for {
		select {
		case <-c.escalated:
			return ErrInterrupted
		default:
		}
		select {
		case r, ok := <-lines:
			if !ok {
				return c.drain()
			}
			if r.err != nil {
				return r.err
			}
			if r.eof {
				return c.drain()
			}
			line := strings.TrimSpace(r.line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "/") {
				cont, err := c.dispatch(ctx, line)
				if err != nil {
					return err
				}
				if !cont {
					return c.drain()
				}
				continue
			}
			if err := c.runTurn(ctx, line); err != nil {
				return err
			}
		case <-ctx.Done():
			c.cancelTurn()
			return nil
		case <-c.escalated:
			return ErrInterrupted
		}
	}
}

// runTurn submits one prompt and renders its completed text to stdout and
// diagnostics to stderr. A ctx cancellation (first interrupt) cancels the
// turn, whose stream ends with the durable terminal event.
func (c *Console) runTurn(ctx context.Context, line string) error {
	turnCtx, cancel := context.WithCancel(ctx)
	c.setTurnCancel(cancel)
	defer c.clearTurnCancel(cancel)

	req := &Request{
		SessionID:   c.sessionID,
		PrincipalID: c.principal,
		Origin:      "terminal",
		Parts:       []Input{{Text: line}},
	}
	var stream []Event
	for ev, err := range c.runner.Run(turnCtx, req) {
		if err != nil {
			_ = writeLinef(c.diag, "aura: %v", err)
			break
		}
		stream = append(stream, ev)
	}
	assistant, diagnostics, terminal := c.render.RenderTurn(stream)
	for _, d := range diagnostics {
		_ = writeLine(c.diag, d)
	}
	if assistant != "" {
		_ = writeLine(c.out, assistant)
	}
	// A cancelled turn has no terminal event in the console's own stream: the
	// engine owns the durable terminal state when the turn context is
	// cancelled by an interrupt. Only a stream that ends without terminality
	// for an un-cancelled turn is an error.
	if !terminal && turnCtx.Err() == nil {
		return errors.New("terminal: turn ended without a terminal event")
	}
	return nil
}

// watchInterrupts feeds interrupts into the run loop: a first interrupt
// cancels the active turn, and a second within the configured window signals
// an escalated exit.
func (c *Console) watchInterrupts(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.interrupts:
			if c.observeInterrupt() {
				select {
				case c.escalated <- struct{}{}:
				case <-ctx.Done():
					return
				}
				return
			}
		}
	}
}

// observeInterrupt records one interrupt and reports whether it escalates a
// second interrupt inside the configured window. The first interrupt always
// cancels the active turn (a no-op when idle); only a second interrupt within
// the window forces the console to exit, which is the durable-cancellation
// contract for first/second Ctrl-C.
func (c *Console) observeInterrupt() bool {
	c.mu.Lock()
	now := c.now()
	armed := !c.lastCancelAt.IsZero() && now.Sub(c.lastCancelAt) < c.config.SecondInterruptTime
	c.lastCancelAt = now
	c.mu.Unlock()
	if armed {
		return true
	}
	c.cancelTurn()
	return false
}

func (c *Console) setTurnCancel(cancel context.CancelFunc) {
	c.mu.Lock()
	c.turnCancel = cancel
	c.mu.Unlock()
}

func (c *Console) clearTurnCancel(cancel context.CancelFunc) {
	c.mu.Lock()
	if c.turnCancel == nil {
		c.mu.Unlock()
		return
	}
	c.turnCancel = nil
	c.mu.Unlock()
	cancel()
}

func (c *Console) cancelTurn() {
	c.mu.Lock()
	cancel := c.turnCancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// drain returns nil at a clean stop; the console has no queued work because
// turns run inline to completion.
func (c *Console) drain() error {
	return nil
}

type readLineResult struct {
	line string
	eof  bool
	err  error
}

func writeLine(w io.Writer, text string) error {
	if _, err := fmt.Fprintln(w, text); err != nil {
		return fmt.Errorf("terminal: write: %w", err)
	}
	return nil
}

func writeLinef(w io.Writer, format string, args ...any) error {
	if _, err := fmt.Fprintf(w, format, args...); err != nil {
		return fmt.Errorf("terminal: write: %w", err)
	}
	return nil
}

// readLines reads newline-delimited prompts until EOF, capping each line at
// maxBytes. Over-long lines fail the console rather than gas up memory.
func readLines(ctx context.Context, r io.Reader, maxBytes int) <-chan readLineResult {
	out := make(chan readLineResult, 1)
	go func() {
		defer close(out)
		br := bufio.NewReaderSize(r, maxBytes+2)
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			line, err := br.ReadString('\n')
			switch {
			case err != nil && !errors.Is(err, io.EOF):
				out <- readLineResult{err: err}
				return
			case len(line) > maxBytes:
				out <- readLineResult{err: errors.New("terminal: input line exceeds the configured maximum")}
				return
			case line == "" && errors.Is(err, io.EOF):
				out <- readLineResult{eof: true}
				return
			case errors.Is(err, io.EOF):
				// Final line without a trailing newline is a prompt.
				out <- readLineResult{line: strings.TrimSuffix(line, "\n")}
				out <- readLineResult{eof: true}
				return
			default:
				out <- readLineResult{line: strings.TrimSuffix(line, "\n")}
			}
		}
	}()
	return out
}

func isSessionConflict(err error) bool {
	type coder interface{ Code() string }
	if c, ok := err.(coder); ok {
		return c.Code() == "session_id_conflict"
	}
	return strings.Contains(err.Error(), "already exists")
}

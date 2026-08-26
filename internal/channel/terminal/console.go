package terminal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

	principal  string
	sessionID  string
	closeInput func()

	mu           sync.Mutex
	interrupts   <-chan struct{}
	escalated    chan struct{}
	now          func() time.Time
	lastCancelAt time.Time
	turnCancel   context.CancelFunc
}

// NewConsole builds a plain console. in/out/diag are owned by the caller; the
// console only closes input when the caller opts in with SetInputCloser.
// principal is the local owner identity stamped on every turn and validated on
// session switches.
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

func (c *Console) SetSessionID(id string) { c.sessionID = id }

// SetInputCloser opts into closing the input reader when Run stops. This lets
// callers release a blocking reader without transferring ownership by default.
func (c *Console) SetInputCloser(closeInput func()) { c.closeInput = closeInput }

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
	var (
		sess Session
		err  error
	)
	if c.sessionID == "" {
		sess, err = c.sessions.Create(ctx, c.principal)
	} else {
		sess, err = c.sessions.Get(ctx, c.sessionID)
		if err == nil && (c.principal == "" || sess.OwnerID != c.principal) {
			return fmt.Errorf("terminal: session %s is not owned by %s", c.sessionID, c.principal)
		}
	}
	if err != nil {
		return fmt.Errorf("terminal: open session: %w", err)
	}
	c.sessionID = sess.ID

	c.escalated = make(chan struct{})
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	if c.interrupts != nil {
		go c.watchInterrupts(watchCtx)
	}

	lines, stopReading := readLines(ctx, c.in, c.config.MaxInputBytes, c.closeInput)
	defer stopReading()
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
	var streamErr error
	var failed, cancelled bool
	for ev, err := range c.runner.Run(turnCtx, req) {
		if err != nil {
			streamErr = err
			if writeErr := writeLinef(c.diag, "aura: %v", err); writeErr != nil {
				return writeErr
			}
			break
		}
		failed = failed || ev.Kind == "turn.failed"
		cancelled = cancelled || ev.Kind == "turn.cancelled"
		stream = appendRenderEvent(stream, ev)
	}
	assistant, diagnostics, terminal := c.render.RenderTurn(stream)
	for _, d := range diagnostics {
		if err := writeLine(c.diag, d); err != nil {
			return err
		}
	}
	// A cancelled turn context suppresses partial output too: the stream may
	// end without a terminal event when cancellation races the executor.
	suppressed := streamErr != nil || failed || cancelled || turnCtx.Err() != nil
	if !suppressed && assistant != "" {
		if err := writeLine(c.out, assistant); err != nil {
			return err
		}
	}
	if streamErr != nil {
		return fmt.Errorf("terminal: turn: %w", streamErr)
	}
	if failed {
		return errors.New("terminal: turn failed")
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
		case _, ok := <-c.interrupts:
			if !ok {
				return
			}
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
	if _, err := fmt.Fprintln(w, sanitizeText(text)); err != nil {
		return fmt.Errorf("terminal: write: %w", err)
	}
	return nil
}

func writeLinef(w io.Writer, format string, args ...any) error {
	return writeLine(w, fmt.Sprintf(format, args...))
}

// readLines reads newline-delimited prompts until EOF, capping each line at
// maxBytes. Over-long lines fail the console rather than gas up memory.
const maxBufferedEvents = 1024

func appendRenderEvent(stream []Event, ev Event) []Event {
	switch ev.Kind {
	case "model.delta", "message.completed":
		payload, err := json.Marshal(struct {
			Text string `json:"text"`
		}{Text: string(decodeDelta(ev.Payload))})
		if err == nil {
			ev.Payload = payload
		}
	default:
		ev.Payload = nil
	}
	if ev.Kind == "model.delta" {
		for i := len(stream) - 1; i >= 0; i-- {
			if stream[i].Kind == "message.completed" {
				break
			}
			if stream[i].Kind == "model.delta" {
				text := append([]byte{}, decodeDelta(stream[i].Payload)...)
				text = append(text, decodeDelta(ev.Payload)...)
				payload, err := json.Marshal(struct {
					Text string `json:"text"`
				}{Text: limitText(string(text), maxRenderBytes)})
				if err == nil {
					stream[i].Payload = payload
				}
				return stream
			}
		}
	}
	if ev.Kind == "message.completed" {
		kept := stream[:0]
		for _, existing := range stream {
			if existing.Kind != "model.delta" {
				kept = append(kept, existing)
			}
		}
		stream = kept
	}
	if len(stream) == maxBufferedEvents {
		drop := 0
		for i, existing := range stream {
			if existing.Kind != "model.delta" && existing.Kind != "message.completed" {
				drop = i
				break
			}
		}
		copy(stream[drop:], stream[drop+1:])
		stream = stream[:maxBufferedEvents-1]
	}
	return append(stream, ev)
}

func readLines(ctx context.Context, r io.Reader, maxBytes int, closeInput func()) (lines <-chan readLineResult, stop func()) {
	out := make(chan readLineResult, 1)
	readCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(out)
		br := bufio.NewReaderSize(r, 4096)
		for {
			select {
			case <-readCtx.Done():
				return
			default:
			}
			line, eof, err := readBoundedLine(br, maxBytes)
			switch {
			case err != nil:
				if !sendLineResult(readCtx, out, readLineResult{err: err}) {
					return
				}
				return
			case eof:
				if line != "" && !sendLineResult(readCtx, out, readLineResult{line: line}) {
					return
				}
				sendLineResult(readCtx, out, readLineResult{eof: true})
				return
			default:
				if !sendLineResult(readCtx, out, readLineResult{line: line}) {
					return
				}
			}
		}
	}()
	stop = func() {
		cancel()
		if closeInput == nil {
			// Without a closer the caller owns the reader and its blocking
			// read; joining would deadlock on a read nothing can release.
			return
		}
		closeInput()
		<-done
	}
	return out, stop
}

func readBoundedLine(br *bufio.Reader, maxBytes int) (lineText string, eof bool, err error) {
	if maxBytes <= 0 {
		return "", false, errors.New("terminal: input line exceeds the configured maximum")
	}
	var line []byte
	for {
		part, err := br.ReadSlice('\n')
		contentLen := len(line) + len(part)
		if newline := bytes.IndexByte(part, '\n'); newline >= 0 {
			contentLen = len(line) + newline
		}
		if contentLen > maxBytes {
			return "", false, errors.New("terminal: input line exceeds the configured maximum")
		}
		line = append(line, part...)
		switch {
		case err == nil:
			line = bytes.TrimSuffix(line, []byte{'\n'})
			line = bytes.TrimSuffix(line, []byte{'\r'})
			return string(line), false, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(line) == 0 {
				return "", true, nil
			}
			line = bytes.TrimSuffix(line, []byte{'\r'})
			return string(line), true, nil
		default:
			return "", false, err
		}
	}
}

func sendLineResult(ctx context.Context, out chan<- readLineResult, result readLineResult) bool {
	select {
	case out <- result:
		return true
	case <-ctx.Done():
		return false
	}
}

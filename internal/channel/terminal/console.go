package terminal

import (
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

	principal    string
	sessionID    string
	closeInput   func()
	tty          *TTYRenderer
	terminalSeen bool

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

// SetTTY switches the console to the interactive presentation: streamed
// frames replace the batch plain renderer, and the multiline editor gesture
// becomes available. nil restores the plain contract.
func (c *Console) SetTTY(r *TTYRenderer) { c.tty = r }

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

	lines, pauseReading, resumeReading, stopReading := readLines(ctx, c.in, c.config.MaxInputBytes, c.closeInput)
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
			ack := func() {
				if r.ack != nil {
					close(r.ack)
				}
			}
			if r.err != nil {
				ack()
				return r.err
			}
			if r.eof {
				ack()
				return c.drain()
			}
			line := strings.TrimSpace(r.line)
			if line == "" {
				ack()
				continue
			}
			// The lone-period gesture composes a multi-line prompt in the
			// user's editor; it is interactive-surface only, and a plain
			// console submits the period as an ordinary prompt.
			if line == "." && c.tty != nil {
				pauseReading()
				ack()
				composed, ok, err := c.tty.Compose(ctx, c.config.MaxInputBytes)
				resumeReading()
				if err != nil {
					if writeErr := writeLinef(c.diag, "aura: %v", err); writeErr != nil {
						return writeErr
					}
					continue
				}
				if ok {
					if err := c.runTurn(ctx, composed); err != nil {
						return err
					}
					continue
				}
				continue
			}
			if strings.HasPrefix(line, "/") {
				ack()
				cont, err := c.dispatch(ctx, line)
				if err != nil {
					return err
				}
				if !cont {
					return c.drain()
				}
				continue
			}
			ack()
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
	c.terminalSeen = false

	req := &Request{
		SessionID:   c.sessionID,
		PrincipalID: c.principal,
		Origin:      "terminal",
		Parts:       []Input{{Text: line}},
	}
	var streamErr error
	var failed, cancelled bool
	if c.tty != nil {
		streamErr = c.streamTurn(turnCtx, req, &failed, &cancelled)
	} else {
		streamErr = c.batchTurn(turnCtx, req, &failed, &cancelled)
	}
	if streamErr != nil {
		return fmt.Errorf("terminal: turn: %w", streamErr)
	}
	if failed {
		return errors.New("terminal: turn failed")
	}
	// A stream that ends without terminality is an error regardless of
	// cancellation state: the turn owns a durable terminal event.
	if !c.terminalSeen {
		return errors.New("terminal: turn ended without a terminal event")
	}
	return nil
}

// streamTurn drives the interactive renderer: events fold into bounded
// render state, a pump coalesces frames at the configured rate, and the
// final frame carries the authoritative completed message.
func (c *Console) streamTurn(turnCtx context.Context, req *Request, failed, cancelled *bool) error {
	c.tty.Begin()
	producerCtx, cancelProducer := context.WithCancel(turnCtx)
	defer cancelProducer()
	pumpCtx, stopPump := context.WithCancel(producerCtx)
	pumpDone := c.tty.StartPump(pumpCtx, func(error) { cancelProducer() })
	var streamErr error
	for ev, err := range c.runner.Run(producerCtx, req) {
		if err != nil {
			streamErr = err
			break
		}
		*failed = *failed || ev.Kind == "turn.failed"
		*cancelled = *cancelled || ev.Kind == "turn.cancelled"
		if ev.Kind == "turn.completed" || ev.Kind == "turn.failed" || ev.Kind == "turn.cancelled" {
			c.terminalSeen = true
		}
		c.tty.Observe(ev)
	}
	stopPump()
	pumpTimer := time.NewTimer(time.Second)
	defer pumpTimer.Stop()
	select {
	case <-pumpDone:
	case <-pumpTimer.C:
		cancelProducer()
		return errors.New("terminal: paint did not stop after cancellation")
	}
	if err := c.tty.Err(); err != nil {
		return err
	}
	if streamErr != nil {
		if err := writeLinef(c.diag, "aura: %v", streamErr); err != nil {
			return err
		}
	}
	finalizeCtx, cancelFinalize := context.WithTimeout(context.WithoutCancel(turnCtx), time.Second)
	defer cancelFinalize()
	if err := c.tty.finalize(finalizeCtx, *failed, *cancelled); err != nil {
		return err
	}
	return streamErr
}

// batchTurn collects the event stream and renders once at the end: the plain
// contract writes only completed assistant text.
func (c *Console) batchTurn(turnCtx context.Context, req *Request, failed, cancelled *bool) error {
	var stream []Event
	retained := 0
	for ev, err := range c.runner.Run(turnCtx, req) {
		if err != nil {
			if writeErr := writeLinef(c.diag, "aura: %v", err); writeErr != nil {
				return writeErr
			}
			return err
		}
		*failed = *failed || ev.Kind == "turn.failed"
		*cancelled = *cancelled || ev.Kind == "turn.cancelled"
		if ev.Kind == "turn.completed" || ev.Kind == "turn.failed" || ev.Kind == "turn.cancelled" {
			c.terminalSeen = true
		}
		stream, retained = appendRenderEvent(stream, ev, retained)
	}
	assistant, diagnostics, terminal := c.render.RenderTurn(stream)
	for _, d := range diagnostics {
		if err := writeLine(c.diag, d); err != nil {
			return err
		}
	}
	// Failed and cancelled turns suppress partial output: the durable log
	// owns what happened, the display shows the outcome.
	suppressed := *failed || *cancelled
	if !suppressed && assistant != "" {
		if err := writeLine(c.out, assistant); err != nil {
			return err
		}
	}
	_ = terminal
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
	ack  chan struct{}
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

// maxBatchStreamBytes bounds the aggregate payload bytes a batch turn may
// retain before rendering; extraction happens at append time so a stream of
// large provider events cannot accumulate a process-sized buffer.
const maxBatchStreamBytes = 2 << 20

func appendRenderEvent(stream []Event, ev Event, retained int) (updated []Event, total int) {
	switch ev.Kind {
	case "model.delta", "message.completed":
		payload, err := json.Marshal(struct {
			Text string `json:"text"`
		}{Text: string(decodeDelta(ev.Payload))})
		if err == nil {
			ev.Payload = payload
		}
	case "adk_event":
		adk, ok := decodeADKEvent(ev.Payload)
		if !ok {
			ev.Payload = nil
			break
		}
		normalized := map[string]any{"partial": adk.Partial}
		var toolCalls, toolResponses []string
		if adk.Content != nil {
			for _, part := range adk.Content.Parts {
				if part.FunctionCall != nil && part.FunctionCall.Name != "" {
					toolCalls = append(toolCalls, limitText(sanitizeText(part.FunctionCall.Name)))
				}
				if part.FunctionResponse != nil && part.FunctionResponse.Name != "" {
					toolResponses = append(toolResponses, limitText(sanitizeText(part.FunctionResponse.Name)))
				}
			}
		}
		if text, present, emptyAssistant := adkText(adk); present {
			normalized["text"] = limitText(sanitizeText(string(text)))
		} else if adk.Content != nil && adk.Content.Role == "user" {
			normalized["role"] = "user"
		} else if emptyAssistant && !adk.Partial {
			normalized["empty"] = true
		}
		if adk.Actions != nil {
			if adk.Actions.TransferToAgent != "" {
				normalized["transfer"] = limitText(sanitizeText(adk.Actions.TransferToAgent))
			}
			if adk.Actions.Escalate {
				normalized["escalate"] = true
			}
		}
		if len(adk.LongRunningToolIDs) > 0 {
			normalized["longRunning"] = len(adk.LongRunningToolIDs)
		}
		if len(toolCalls) > 0 {
			normalized["toolCalls"] = toolCalls
		}
		if len(toolResponses) > 0 {
			normalized["toolResponses"] = toolResponses
		}
		if payload, err := json.Marshal(normalized); err == nil {
			ev.Payload = payload
		} else {
			ev.Payload = nil
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
				before := len(stream[i].Payload)
				text := append([]byte{}, decodeDelta(stream[i].Payload)...)
				text = append(text, decodeDelta(ev.Payload)...)
				payload, err := json.Marshal(struct {
					Text string `json:"text"`
				}{Text: limitText(string(text))})
				if err == nil {
					stream[i].Payload = payload
					retained += len(payload) - before
				}
				stream, retained = trimBatchStream(stream, retained)
				return stream, retained
			}
		}
	}
	if ev.Kind == "message.completed" {
		kept := stream[:0]
		for _, existing := range stream {
			if existing.Kind == "model.delta" {
				retained -= len(existing.Payload)
				continue
			}
			kept = append(kept, existing)
		}
		stream = kept
	}
	stream, retained = trimBatchStream(stream, retained)
	if retained+len(ev.Payload) > maxBatchStreamBytes {
		ev.Payload = nil
	}
	return append(stream, ev), retained + len(ev.Payload)
}

func trimBatchStream(stream []Event, retained int) (updated []Event, total int) {
	for len(stream) > 0 && (len(stream) >= maxBufferedEvents || retained > maxBatchStreamBytes) {
		drop := 0
		for i, existing := range stream {
			if existing.Kind != "model.delta" && existing.Kind != "message.completed" {
				drop = i
				break
			}
		}
		retained -= len(stream[drop].Payload)
		copy(stream[drop:], stream[drop+1:])
		stream = stream[:len(stream)-1]
	}
	return stream, retained
}

// readLines reads newline-delimited prompts until EOF. pause stops reading
// between lines and releases stdin to another consumer (the multiline editor
// gesture); resume restarts reading afterwards. Both are safe on a stopped
// reader.
func readLines(ctx context.Context, r io.Reader, maxBytes int, closeInput func()) (lines <-chan readLineResult, pause, resume, stop func()) {
	out := make(chan readLineResult)
	readCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var gateMu sync.Mutex
	var gate chan struct{} // nil while running; closed to resume
	go func() {
		defer close(done)
		defer close(out)
		for {
			gateMu.Lock()
			g := gate
			gateMu.Unlock()
			if g != nil {
				select {
				case <-readCtx.Done():
					return
				case <-g:
				}
			}
			select {
			case <-readCtx.Done():
				return
			default:
			}
			line, eof, err := readBoundedLine(r, maxBytes)
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
	pause = func() {
		gateMu.Lock()
		defer gateMu.Unlock()
		if gate == nil {
			gate = make(chan struct{})
		}
	}
	resume = func() {
		gateMu.Lock()
		defer gateMu.Unlock()
		if gate != nil {
			close(gate)
			gate = nil
		}
	}
	stop = func() {
		cancel()
		resume()
		if closeInput == nil {
			// Without a closer the caller owns the reader and its blocking
			// read; joining would deadlock on a read nothing can release.
			return
		}
		closeInput()
		<-done
	}
	return out, pause, resume, stop
}

func readBoundedLine(r io.Reader, maxBytes int) (lineText string, eof bool, err error) {
	if maxBytes <= 0 {
		return "", false, errors.New("terminal: input line exceeds the configured maximum")
	}
	var line []byte
	var one [1]byte
	for {
		n, readErr := r.Read(one[:])
		if n > 0 {
			if one[0] == '\n' {
				line = bytes.TrimSuffix(line, []byte{'\r'})
				return string(line), false, nil
			}
			line = append(line, one[0])
			if len(line) > maxBytes {
				return "", false, errors.New("terminal: input line exceeds the configured maximum")
			}
		}
		switch {
		case readErr == nil:
			continue
		case errors.Is(readErr, io.EOF):
			if len(line) == 0 {
				return "", true, nil
			}
			line = bytes.TrimSuffix(line, []byte{'\r'})
			return string(line), true, nil
		default:
			return "", false, readErr
		}
	}
}

func sendLineResult(ctx context.Context, out chan<- readLineResult, result readLineResult) bool {
	ack := make(chan struct{})
	result.ack = ack
	select {
	case out <- result:
	case <-ctx.Done():
		return false
	}
	select {
	case <-ack:
		return true
	case <-ctx.Done():
		return false
	}
}

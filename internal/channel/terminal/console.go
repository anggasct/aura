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

var ErrInterrupted = errors.New("terminal: interrupted")

type Console struct {
	runner   Runner
	sessions Sessions
	render   Renderer

	in     io.Reader
	out    io.Writer
	diag   io.Writer
	config Config

	principal           string
	sessionID           string
	closeInput          func()
	tty                 *TTYRenderer
	terminalSeen        bool
	approvals           *ApprovalBridge
	lines               <-chan readLineResult
	discardNextApproval bool

	mu           sync.Mutex
	interrupts   <-chan struct{}
	escalated    chan struct{}
	now          func() time.Time
	lastCancelAt time.Time
	turnCancel   context.CancelFunc
}

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

func (c *Console) SetClock(now func() time.Time) { c.now = now }

func (c *Console) SetInterrupts(ch <-chan struct{}) { c.interrupts = ch }

func (c *Console) SetSessionID(id string) { c.sessionID = id }

func (c *Console) SetInputCloser(closeInput func()) { c.closeInput = closeInput }

func (c *Console) SetTTY(r *TTYRenderer) { c.tty = r }

func (c *Console) SetApprovalBridge(b *ApprovalBridge) { c.approvals = b }

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
	c.lines = lines
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
			if c.discardNextApproval {
				c.discardNextApproval = false
				ack()
				continue
			}
			line := strings.TrimSpace(r.line)
			if line == "" {
				ack()
				continue
			}
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
	if !c.terminalSeen {
		return errors.New("terminal: turn ended without a terminal event")
	}
	return nil
}

type streamItem struct {
	ev  Event
	err error
}

func (c *Console) streamTurn(turnCtx context.Context, req *Request, failed, cancelled *bool) error {
	c.tty.Begin()
	producerCtx, cancelProducer := context.WithCancel(turnCtx)
	defer cancelProducer()
	pumpCtx, stopPump := context.WithCancel(producerCtx)
	pumpDone := c.tty.StartPump(pumpCtx, func(error) { cancelProducer() })

	consumeCtx, stopConsuming := context.WithCancel(context.Background())
	defer stopConsuming()
	items := make(chan streamItem)
	produceDone := make(chan struct{})
	go func() {
		defer close(produceDone)
		defer close(items)
		for ev, err := range c.runner.Run(producerCtx, req) {
			select {
			case items <- streamItem{ev: ev, err: err}:
			case <-consumeCtx.Done():
				return
			}
		}
	}()

	var approvalReady <-chan struct{}
	if c.approvals != nil {
		approvalReady = c.approvals.readyCh()
	}
	var streamErr error
	var serving *approvalAsk
	var answers <-chan readLineResult
	var expiry <-chan time.Time
	var turnDone <-chan struct{}
	var expiryTimer *time.Timer
	answer := func(accepted bool, outcome string) {
		if serving == nil {
			return
		}
		card := serving.card
		serving.answer(accepted)
		if expiryTimer != nil {
			expiryTimer.Stop()
			expiryTimer = nil
		}
		expiry, turnDone, answers, serving = nil, nil, nil, nil
		writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(turnCtx), time.Second)
		defer cancelWrite()
		line := fmt.Sprintf("approval %s: %s@%s\n", outcome, sanitizeText(card.ToolName), sanitizeText(card.ToolVersion))
		if err := c.tty.DirectWrite(writeCtx, line); err != nil {
			c.tty.setErr(err)
		}
	}
	rejectPending := func(outcome string) {
		if serving != nil {
			c.discardNextApproval = true
		}
		answer(false, outcome)
	}
loop:
	for {
		select {
		case item, ok := <-items:
			if !ok {
				break loop
			}
			if item.err != nil {
				streamErr = item.err
				break loop
			}
			ev := item.ev
			*failed = *failed || ev.Kind == "turn.failed"
			*cancelled = *cancelled || ev.Kind == "turn.cancelled"
			if ev.Kind == "turn.completed" || ev.Kind == "turn.failed" || ev.Kind == "turn.cancelled" {
				c.terminalSeen = true
			}
			c.tty.Observe(ev)
		case <-approvalReady:
			ask := c.approvals.take()
			if ask == nil {
				continue
			}
			cardWidth := 0
			if c.tty.opt.Width != nil {
				cardWidth = c.tty.opt.Width()
			}
			writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(turnCtx), time.Second)
			err := c.tty.DirectWrite(writeCtx, renderApprovalCard(ask.card, cardWidth))
			cancelWrite()
			if err != nil {
				c.tty.setErr(err)
				c.discardNextApproval = true
				ask.answer(false)
				streamErr = err
				break loop
			}
			serving = ask
			answers = c.lines
			turnDone = turnCtx.Done()
			if !ask.card.ExpiresAt.IsZero() {
				delay := time.Until(ask.card.ExpiresAt)
				if delay < 0 {
					delay = 0
				}
				expiryTimer = time.NewTimer(delay)
				expiry = expiryTimer.C
			}
		case r, ok := <-answers:
			if !ok {
				answers = nil
				answer(false, "rejected (input closed)")
				continue
			}
			if r.ack != nil {
				close(r.ack)
			}
			if r.err != nil || r.eof {
				answer(false, "rejected (input closed)")
				continue
			}
			if approvalAccepted(r.line) {
				answer(true, "accepted")
			} else {
				answer(false, "rejected")
			}
		case <-expiry:
			rejectPending("rejected (expired)")
		case <-turnDone:
			rejectPending("rejected (turn ended)")
		}
	}
	rejectPending("rejected (turn ended)")
	if expiryTimer != nil {
		expiryTimer.Stop()
	}
	stopPump()
	stopConsuming()
	cancelProducer()
	pumpTimer := time.NewTimer(5 * time.Second)
	defer pumpTimer.Stop()
	select {
	case <-pumpDone:
	case <-pumpTimer.C:
		return errors.New("terminal: paint did not stop after cancellation")
	}
	select {
	case <-produceDone:
	case <-pumpTimer.C:
		return errors.New("terminal: event pump did not stop after cancellation")
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
	suppressed := *failed || *cancelled
	if !suppressed && assistant != "" {
		if err := writeLine(c.out, assistant); err != nil {
			return err
		}
	}
	_ = terminal
	return nil
}

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

const maxBufferedEvents = 1024

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

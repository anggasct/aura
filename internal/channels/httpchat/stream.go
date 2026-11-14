package httpchat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

const (
	chunkObject            = "chat.completion.chunk"
	contentTypeEventStream = "text/event-stream"
	frameDataPrefix        = "data: "
	frameEnd               = "\n\n"
	frameKeepalive         = ": keepalive\n\n"
	frameDone              = "data: [DONE]\n\n"
	writeDeadline          = 10 * time.Second
)

type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *usage        `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type turnItem struct {
	event store.RuntimeEvent
	err   error
}

type eventStream struct {
	w         http.ResponseWriter
	rc        *http.ResponseController
	committed bool
	writes    int
}

func newEventStream(w http.ResponseWriter) *eventStream {
	return &eventStream{w: w, rc: http.NewResponseController(w)}
}

func (s *eventStream) armDeadline() error {
	err := s.rc.SetWriteDeadline(time.Now().Add(writeDeadline))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

func (s *eventStream) commit() error {
	if s.committed {
		return nil
	}
	if err := s.armDeadline(); err != nil {
		return err
	}
	header := s.w.Header()
	header.Set("Content-Type", contentTypeEventStream)
	header.Set("Cache-Control", "no-cache")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.committed = true
	return s.rc.Flush()
}

func (s *eventStream) write(frame string) error {
	if err := s.commit(); err != nil {
		return err
	}
	if err := s.armDeadline(); err != nil {
		return err
	}
	if _, err := s.w.Write([]byte(frame)); err != nil {
		return err
	}
	s.writes++
	return s.rc.Flush()
}

func (s *eventStream) writeData(body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return s.write(frameDataPrefix + string(payload) + frameEnd)
}

type streamRun struct {
	chat         *Chat
	w            http.ResponseWriter
	caller       *Principal
	es           *eventStream
	includeUsage bool
	created      int64
	turnID       string
	events       []store.RuntimeEvent
	live         *liveMessage
	nextDelta    uint64
	answered     strings.Builder
	answers      int
	messages     int
	roleSent     bool
}

type liveMessage struct {
	text   strings.Builder
	opened bool
	lost   bool
}

func (c *Chat) stream(ctx context.Context, w http.ResponseWriter, caller *Principal, turnRequest *turn.TurnRequest, includeUsage bool) {
	runCtx, cancel := context.WithCancel(ctx)
	items := make(chan turnItem)
	var pump sync.WaitGroup
	pump.Go(func() { c.pump(runCtx, turnRequest, items) })
	defer pump.Wait()
	defer cancel()

	run := &streamRun{chat: c, w: w, caller: caller, es: newEventStream(w), includeUsage: includeUsage, created: time.Now().Unix(), live: &liveMessage{}}
	idle := time.NewTimer(c.keepalive)
	defer idle.Stop()
	for {
		wrote := run.es.writes
		select {
		case <-ctx.Done():
			c.logger.InfoContext(ctx, "http chat client left before the turn ended", "component", "http")
			return
		case <-idle.C:
			if err := run.es.write(frameKeepalive); err != nil {
				c.abandon(ctx, err)
				return
			}
		case item, open := <-items:
			if ctx.Err() != nil {
				c.logger.InfoContext(ctx, "http chat client left before the turn ended", "component", "http")
				return
			}
			finished, err := run.consume(ctx, &item, open)
			if err != nil {
				c.abandon(ctx, err)
				return
			}
			if finished {
				return
			}
		}
		if run.es.writes != wrote {
			idle.Reset(c.keepalive)
		}
	}
}

func (c *Chat) abandon(ctx context.Context, err error) {
	c.logger.InfoContext(ctx, "http chat stream write failed", "component", "http", "error", err)
}

func (c *Chat) pump(ctx context.Context, turnRequest *turn.TurnRequest, items chan<- turnItem) {
	defer close(items)
	for event, err := range c.runtime.Run(ctx, turnRequest) {
		select {
		case items <- turnItem{event: event, err: err}:
		case <-ctx.Done():
			return
		}
	}
}

func (r *streamRun) consume(ctx context.Context, item *turnItem, open bool) (bool, error) {
	if !open {
		r.chat.logger.WarnContext(ctx, "http chat turn ended without a result", "component", "http")
		return true, r.refuse(ctx, &errInternal, "turn_incomplete")
	}
	if item.err != nil {
		code, _ := fault.CodeOf[turn.ErrorCode](item.err)
		return true, r.fail(ctx, code)
	}
	event := &item.event
	if r.turnID == "" {
		r.turnID = event.TurnID
	}
	if event.Kind == turn.EventKindModelDelta {
		return false, r.delta(event)
	}
	r.events = append(r.events, *event)
	switch event.Kind {
	case turn.EventKindTurnFailed:
		return true, r.fail(ctx, failureCode(event.Payload))
	case turn.EventKindTurnCancelled:
		r.chat.logger.WarnContext(ctx, "http chat turn ended without a result", "component", "http", "terminal", event.Kind)
		return true, r.refuse(ctx, &errInternal, "turn_incomplete")
	}
	if err := r.es.commit(); err != nil {
		return true, err
	}
	consistent, err := r.deliver(event)
	if err != nil {
		return true, err
	}
	if !consistent {
		return true, r.refuse(ctx, &errInternal, "stream_diverged")
	}
	if event.Kind == turn.EventKindTurnCompleted {
		return true, r.finish(ctx)
	}
	return false, nil
}

func (r *streamRun) fail(ctx context.Context, code turn.ErrorCode) error {
	r.chat.logger.WarnContext(ctx, "http chat turn failed", "component", "http", "code", string(code))
	if !r.es.committed {
		r.chat.rejectFailure(ctx, r.w, code)
		return nil
	}
	spec, reason := failureSpec(code)
	return r.errorEvent(ctx, spec, reason)
}

func (r *streamRun) refuse(ctx context.Context, spec *errorSpec, reason string) error {
	if !r.es.committed {
		r.chat.reject(ctx, r.w, spec, reason)
		return nil
	}
	return r.errorEvent(ctx, spec, reason)
}

func (r *streamRun) errorEvent(ctx context.Context, spec *errorSpec, reason string) error {
	r.chat.logger.WarnContext(ctx, "http stream failed", "component", "http", "reason", reason, "status", spec.status)
	return r.es.writeData(newErrorEnvelope(spec))
}

func (r *streamRun) delta(event *store.RuntimeEvent) error {
	delta, ok := turn.DecodeModelDelta(event.Payload)
	if !ok || delta.Index != r.nextDelta {
		r.live.lost = true
	}
	if ok {
		r.nextDelta = delta.Index + 1
	}
	if r.live.lost || delta.Text == "" {
		return nil
	}
	piece := delta.Text
	if !r.live.opened {
		r.live.opened = true
		piece = r.openMessage() + piece
	}
	r.live.text.WriteString(delta.Text)
	return r.content(piece)
}

func (r *streamRun) openMessage() string {
	separator := ""
	if r.messages > 0 {
		separator = answerSeparator
	}
	r.messages++
	return separator
}

func (r *streamRun) deliver(event *store.RuntimeEvent) (consistent bool, err error) {
	live := r.live
	if event.Kind == store.EventKindADK {
		r.live = &liveMessage{}
	}
	for i, text := range r.chat.answerTexts(r.events[len(r.events)-1:]) {
		if r.answers > 0 {
			r.answered.WriteString(answerSeparator)
		}
		r.answered.WriteString(text)
		r.answers++
		var piece string
		if i == 0 && live.opened {
			rest, found := strings.CutPrefix(text, live.text.String())
			if !found {
				return false, nil
			}
			piece = rest
		} else {
			piece = r.openMessage() + text
		}
		if err := r.content(piece); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (r *streamRun) content(piece string) error {
	if piece == "" {
		return nil
	}
	delta := chunkDelta{Content: piece}
	if !r.roleSent {
		delta.Role = roleAssistant
		r.roleSent = true
	}
	return r.chunk([]chunkChoice{{Delta: delta}}, nil)
}

func (r *streamRun) chunk(choices []chunkChoice, consumed *usage) error {
	return r.es.writeData(&chunk{
		ID:      "chatcmpl-" + r.turnID,
		Object:  chunkObject,
		Created: r.created,
		Model:   r.caller.PersonaID,
		Choices: choices,
		Usage:   consumed,
	})
}

func (r *streamRun) finish(ctx context.Context) error {
	answer := strings.Join(r.chat.answerTexts(r.events), answerSeparator)
	rest, consistent := strings.CutPrefix(answer, r.answered.String())
	if !consistent {
		return r.refuse(ctx, &errInternal, "stream_diverged")
	}
	if err := r.content(rest); err != nil {
		return err
	}
	if !r.roleSent {
		r.roleSent = true
		if err := r.chunk([]chunkChoice{{Delta: chunkDelta{Role: roleAssistant}}}, nil); err != nil {
			return err
		}
	}
	reason := finishReason(r.events)
	if err := r.chunk([]chunkChoice{{FinishReason: &reason}}, nil); err != nil {
		return err
	}
	if r.includeUsage {
		consumed := tokenUsage(r.events)
		if err := r.chunk([]chunkChoice{}, &consumed); err != nil {
			return err
		}
	}
	return r.es.write(frameDone)
}

package httpchat

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

func TestStreamWritesRoleContentFinishAndDone(t *testing.T) {
	fixture := newChatFixture(t, chatFixtureOptions{})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

	rec := fixture.post(t, issued.Plaintext, defaultStreamBody())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Type":           "text/event-stream",
		"Cache-Control":          "no-cache",
		"X-Content-Type-Options": "nosniff",
		"X-Accel-Buffering":      "no",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if !strings.HasPrefix(rec.Header().Get(SessionHeader), testPersona+"/http/"+issued.ID+"/") {
		t.Errorf("session header = %q", rec.Header().Get(SessionHeader))
	}
	frames := sseFrames(t, rec.Body.String())
	if len(frames) != 3 || frames[2] != frameDoneMarker {
		t.Fatalf("frames = %q, want content, finish and DONE", frames)
	}
	content, finish := decodeChunk(t, frames[0]), decodeChunk(t, frames[1])
	if content.ID != "chatcmpl-"+testTurnID || content.Object != "chat.completion.chunk" || content.Model != testPersona || content.Created == 0 {
		t.Errorf("content chunk = %+v", content)
	}
	if len(content.Choices) != 1 || content.Choices[0].Delta.Role != "assistant" || content.Choices[0].Delta.Content != testReply || content.Choices[0].FinishReason != nil {
		t.Errorf("content choices = %+v", content.Choices)
	}
	if finish.ID != content.ID || finish.Created != content.Created || finish.Object != "chat.completion.chunk" || finish.Usage != nil {
		t.Errorf("finish chunk = %+v", finish)
	}
	if len(finish.Choices) != 1 || finish.Choices[0].Delta.Role != "" || finish.Choices[0].Delta.Content != "" ||
		finish.Choices[0].FinishReason == nil || *finish.Choices[0].FinishReason != "stop" {
		t.Errorf("finish choices = %+v", finish.Choices)
	}
	if strings.Contains(frames[1], `"delta":null`) || !strings.Contains(frames[1], `"delta":{}`) {
		t.Errorf("finish frame must carry an empty delta object: %s", frames[1])
	}
}

func TestStreamUsageChunkFollowsOnlyWhenRequested(t *testing.T) {
	cases := map[string]struct {
		include bool
		frames  int
	}{
		"without stream options": {include: false, frames: 3},
		"with include usage":     {include: true, frames: 4},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newChatFixture(t, chatFixtureOptions{})
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

			rec := fixture.post(t, issued.Plaintext, streamBody(tc.include, userMessage("hi")))

			frames := sseFrames(t, rec.Body.String())
			if len(frames) != tc.frames || frames[len(frames)-1] != frameDoneMarker {
				t.Fatalf("frames = %q, want %d ending in DONE", frames, tc.frames)
			}
			if !tc.include {
				return
			}
			last := decodeChunk(t, frames[2])
			if last.Choices == nil || len(last.Choices) != 0 || !strings.Contains(frames[2], `"choices":[]`) {
				t.Errorf("usage chunk choices = %+v in %s", last.Choices, frames[2])
			}
			if last.Usage == nil || last.Usage.PromptTokens != 10 || last.Usage.CompletionTokens != 5 || last.Usage.TotalTokens != 15 {
				t.Errorf("usage chunk = %+v", last.Usage)
			}
		})
	}
}

func TestStreamedTextEqualsTheNonStreamingText(t *testing.T) {
	partials := func(events []store.RuntimeEvent) []string {
		if texts := testAnswerTexts(events); len(texts) > 0 {
			return texts
		}
		var texts []string
		for i := range events {
			if events[i].Kind == "model.partial" {
				texts = append(texts, string(events[i].Payload))
			}
		}
		return texts
	}
	cases := map[string]struct {
		script  scriptRuntime
		answers func(events []store.RuntimeEvent) []string
		want    string
	}{
		"single message": {
			script: fixedTurn(turnEvent(turn.EventKindTurnAccepted), answerEvent("hello"), turnEvent(turn.EventKindTurnCompleted)),
			want:   "hello",
		},
		"messages around a tool call": {
			script: fixedTurn(
				turnEvent(turn.EventKindTurnAccepted), answerEvent("first"),
				turnEvent(turn.EventKindToolStarted), turnEvent(turn.EventKindToolCompleted),
				answerEvent("second"), turnEvent(turn.EventKindTurnCompleted)),
			want: "first\n\nsecond",
		},
		"empty answer": {
			script: fixedTurn(turnEvent(turn.EventKindTurnAccepted), turnEvent(turn.EventKindTurnCompleted)),
			want:   "",
		},
		"empty text between answers": {
			script: fixedTurn(
				turnEvent(turn.EventKindTurnAccepted), answerEvent(""), answerEvent("late"),
				turnEvent(turn.EventKindTurnCompleted)),
			want: "\n\nlate",
		},
		"fallback to unfinished text": {
			script: fixedTurn(
				turnEvent(turn.EventKindTurnAccepted),
				store.RuntimeEvent{TurnID: testTurnID, Kind: "model.partial", Payload: []byte("par")},
				store.RuntimeEvent{TurnID: testTurnID, Kind: "model.partial", Payload: []byte("tial")},
				turnEvent(turn.EventKindTurnCompleted)),
			answers: partials,
			want:    "par\n\ntial",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newChatFixture(t, chatFixtureOptions{backend: tc.script, answers: tc.answers})
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

			whole := decodeCompletion(t, fixture.post(t, issued.Plaintext, chatBody(userMessage("hi"))))
			streamed := fixture.post(t, issued.Plaintext, defaultStreamBody())

			frames := sseFrames(t, streamed.Body.String())
			if got := contentOf(t, frames); got != tc.want || whole.Choices[0].Message.Content != tc.want {
				t.Errorf("streamed %q, non-streaming %q, want %q", got, whole.Choices[0].Message.Content, tc.want)
			}
			if first := decodeChunk(t, frames[0]); first.Choices[0].Delta.Role != "assistant" {
				t.Errorf("first chunk has role %q, want assistant", first.Choices[0].Delta.Role)
			}
			if frames[len(frames)-1] != frameDoneMarker {
				t.Errorf("stream does not end with DONE: %q", frames)
			}
		})
	}
}

func TestStreamKeepsGateErrorsAsPlainJSON(t *testing.T) {
	cases := map[string]struct {
		setup      func(f *chatFixture)
		body       string
		headers    []string
		status     int
		kind       string
		code       string
		param      string
		retryAfter string
	}{
		"invalid body": {
			body: `{"stream":true,"messages":[]}`, status: http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request", param: "messages",
		},
		"model mismatch": {
			body: `{"model":"other","stream":true,"messages":[{"role":"user","content":"hi"}]}`, status: http.StatusForbidden,
			kind: errorTypePermission, code: "persona_not_permitted", param: "model",
		},
		"unknown session": {
			body: defaultStreamBody(), headers: []string{SessionHeader, testPersona + "/http/aura_key_x/AAAAAAAAAAAAAAAAAAAAAAAAAA"},
			status: http.StatusNotFound, kind: errorTypeInvalidRequest, code: "session_not_found", param: "X-Aura-Session",
		},
		"budget exhausted": {
			setup: func(f *chatFixture) { f.budgets.refusal = access.BudgetRefusalExhausted }, body: defaultStreamBody(),
			status: http.StatusTooManyRequests, kind: "insufficient_quota", code: "budget_exhausted",
		},
		"turn rate exceeded": {
			setup: func(f *chatFixture) { f.budgets.refusal = access.BudgetRefusalRateLimited }, body: defaultStreamBody(),
			status: http.StatusTooManyRequests, kind: errorTypeRateLimit, code: "rate_limit_exceeded", retryAfter: "60",
		},
		"persona not evaluated": {
			setup: func(f *chatFixture) { f.evals.evaluated = false }, body: defaultStreamBody(),
			status: http.StatusForbidden, kind: errorTypePermission, code: "eval_gate_closed",
		},
		"runtime overloaded at admission": {
			setup: func(f *chatFixture) {
				f.runtime.runErr = &turn.Error{Code: turn.ErrorCodeRuntimeOverloaded, Detail: "queue is full"}
			},
			body: defaultStreamBody(), status: http.StatusServiceUnavailable, kind: errorTypeServer, code: "runtime_overloaded", retryAfter: "1",
		},
		"admission failure": {
			setup: func(f *chatFixture) { f.runtime.runErr = errors.New("boom with prompt text") }, body: defaultStreamBody(),
			status: http.StatusInternalServerError, kind: errorTypeServer, code: "internal_error",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newChatFixture(t, chatFixtureOptions{})
			if tc.setup != nil {
				tc.setup(fixture)
			}
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

			rec := fixture.post(t, issued.Plaintext, tc.body, tc.headers...)

			expectCoded(t, rec, tc.status, tc.kind, tc.code, tc.param)
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("content type = %q, want application/json", got)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tc.retryAfter)
			}
			for _, leaked := range []string{"boom", "prompt text", "queue is full"} {
				if strings.Contains(rec.Body.String(), leaked) || strings.Contains(fixture.logs.String(), leaked) {
					t.Errorf("rejection leaked %q", leaked)
				}
			}
		})
	}
}

func TestStreamAudienceGateAnswersPlainJSON(t *testing.T) {
	fixture := newChatFixture(t, chatFixtureOptions{audience: config.AudienceOwner})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

	rec := fixture.post(t, issued.Plaintext, defaultStreamBody())

	expectCoded(t, rec, http.StatusForbidden, errorTypePermission, "audience_denied", "")
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("content type = %q", rec.Header().Get("Content-Type"))
	}
}

func TestStreamFirstEventFailureKeepsItsStatus(t *testing.T) {
	cases := map[string]struct {
		code   string
		status int
		kind   string
		want   string
	}{
		"budget exhausted":   {code: "budget_exhausted", status: http.StatusTooManyRequests, kind: "insufficient_quota", want: "budget_exhausted"},
		"runtime overloaded": {code: "runtime_overloaded", status: http.StatusServiceUnavailable, kind: errorTypeServer, want: "runtime_overloaded"},
		"unknown":            {code: "brand_new_code", status: http.StatusInternalServerError, kind: errorTypeServer, want: "internal_error"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newChatFixture(t, chatFixtureOptions{backend: fixedTurn(failedEvent(tc.code))})
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

			rec := fixture.post(t, issued.Plaintext, defaultStreamBody())

			expectCoded(t, rec, tc.status, tc.kind, tc.want, "")
			if rec.Header().Get(SessionHeader) == "" {
				t.Error("a failed turn lost its session header")
			}
		})
	}
}

func TestStreamFailureAfterTheStreamStartedEmitsOneErrorEvent(t *testing.T) {
	cases := map[string]struct {
		code string
		kind string
		want string
	}{
		"budget exhausted":   {code: "budget_exhausted", kind: "insufficient_quota", want: "budget_exhausted"},
		"runtime overloaded": {code: "runtime_overloaded", kind: errorTypeServer, want: "runtime_overloaded"},
		"runtime internal":   {code: "runtime_internal", kind: errorTypeServer, want: "internal_error"},
		"unknown code":       {code: "brand_new_code", kind: errorTypeServer, want: "internal_error"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			script := fixedTurn(turnEvent(turn.EventKindTurnAccepted), answerEvent("partial answer"), failedEvent(tc.code), turnEvent(turn.EventKindTurnCompleted))
			fixture := newChatFixture(t, chatFixtureOptions{backend: script})
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

			rec := fixture.post(t, issued.Plaintext, defaultStreamBody())

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 once the stream started", rec.Code)
			}
			frames := sseFrames(t, rec.Body.String())
			if len(frames) != 2 {
				t.Fatalf("frames = %q, want one chunk and one error event", frames)
			}
			if got := contentOf(t, frames[:1]); got != "partial answer" {
				t.Errorf("streamed %q before the failure", got)
			}
			failure := errorEventOf(t, frames[1])
			if failure.Error.Type != tc.kind || failure.Error.Code != tc.want || failure.Error.Message == "" {
				t.Errorf("error event = %+v, want %s/%s", failure.Error, tc.kind, tc.want)
			}
			if strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
				t.Errorf("failed stream was closed as complete: %s", rec.Body.String())
			}
			for _, leaked := range []string{"internal detail", "secret model output", "prompt"} {
				if strings.Contains(frames[1], leaked) || strings.Contains(fixture.logs.String(), leaked) {
					t.Errorf("failure leaked %q", leaked)
				}
			}
		})
	}
}

func TestStreamTurnWithoutAResultEmitsAnInternalErrorEvent(t *testing.T) {
	diverging := func(events []store.RuntimeEvent) []string {
		if len(events) == 1 {
			if events[0].Kind == turn.EventKindMessageCompleted {
				return []string{"streamed"}
			}
			return nil
		}
		return []string{"another text"}
	}
	cases := map[string]struct {
		script  scriptRuntime
		answers func(events []store.RuntimeEvent) []string
		frames  int
	}{
		"cancelled turn": {
			script: fixedTurn(turnEvent(turn.EventKindTurnAccepted), turnEvent(turn.EventKindTurnCancelled)),
			frames: 1,
		},
		"stream ends without a terminal event": {
			script: fixedTurn(turnEvent(turn.EventKindTurnAccepted), answerEvent("cut off")),
			frames: 2,
		},
		"iterator error after events": {
			script: func(_ context.Context, yield func(store.RuntimeEvent, error) bool) {
				if emit(yield, turnEvent(turn.EventKindTurnAccepted)) {
					yield(store.RuntimeEvent{}, errors.New("boom with prompt text"))
				}
			},
			frames: 1,
		},
		"final answer diverges from the streamed text": {
			script:  fixedTurn(turnEvent(turn.EventKindTurnAccepted), answerEvent("one"), turnEvent(turn.EventKindTurnCompleted)),
			answers: diverging,
			frames:  2,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newChatFixture(t, chatFixtureOptions{backend: tc.script, answers: tc.answers})
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

			rec := fixture.post(t, issued.Plaintext, defaultStreamBody())

			frames := sseFrames(t, rec.Body.String())
			if len(frames) != tc.frames {
				t.Fatalf("frames = %q, want %d", frames, tc.frames)
			}
			failure := errorEventOf(t, frames[len(frames)-1])
			if failure.Error.Type != errorTypeServer || failure.Error.Code != "internal_error" {
				t.Errorf("error event = %+v", failure.Error)
			}
			if strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), "boom") || strings.Contains(fixture.logs.String(), "boom") {
				t.Errorf("unsafe stream end: %s", rec.Body.String())
			}
		})
	}
}

func TestStreamKeepaliveFlowsWhileATurnIsQuiet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		script := scriptRuntime(func(ctx context.Context, yield func(store.RuntimeEvent, error) bool) {
			if !emit(yield, turnEvent(turn.EventKindTurnAccepted), turnEvent(turn.EventKindToolStarted)) {
				return
			}
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
			emit(yield, turnEvent(turn.EventKindToolCompleted), answerEvent("done"), turnEvent(turn.EventKindTurnCompleted))
		})
		fixture := newChatFixture(t, chatFixtureOptions{backend: script})
		issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
		w := newCaptureWriter()
		served := fixture.serveAsync(jsonPost(t, issued.Plaintext, defaultStreamBody()).WithContext(t.Context()), w)

		time.Sleep(testKeepalive / 2)
		synctest.Wait()
		if status, body := w.snapshot(); status != http.StatusOK || body != "" {
			t.Fatalf("before the interval: status %d body %q, want headers only", status, body)
		}
		time.Sleep(testKeepalive)
		synctest.Wait()
		if _, body := w.snapshot(); body != frameKeepalives+"\n\n" {
			t.Fatalf("after one interval: body %q", body)
		}
		time.Sleep(testKeepalive)
		synctest.Wait()
		if _, body := w.snapshot(); body != strings.Repeat(frameKeepalives+"\n\n", 2) {
			t.Fatalf("after two intervals: body %q", body)
		}
		close(release)
		synctest.Wait()

		select {
		case <-served:
		default:
			t.Fatal("handler is still running after the turn completed")
		}
		_, body := w.snapshot()
		frames := sseFrames(t, body)
		if len(frames) != 5 || frames[0] != frameKeepalives || frames[1] != frameKeepalives || frames[4] != frameDoneMarker {
			t.Fatalf("frames = %q, want two keepalives, content, finish and DONE", frames)
		}
		if got := contentOf(t, frames); got != "done" {
			t.Errorf("streamed %q, want done", got)
		}
		if w.header.Get("Content-Type") != "text/event-stream" {
			t.Errorf("content type = %q", w.header.Get("Content-Type"))
		}
	})
}

func TestStreamKeepaliveWaitsForAQuietInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		step := make(chan struct{})
		release := make(chan struct{})
		script := scriptRuntime(func(ctx context.Context, yield func(store.RuntimeEvent, error) bool) {
			if !emit(yield, turnEvent(turn.EventKindTurnAccepted)) {
				return
			}
			select {
			case <-step:
			case <-ctx.Done():
				return
			}
			if !emit(yield, answerEvent("early")) {
				return
			}
			select {
			case <-release:
			case <-ctx.Done():
				return
			}
			emit(yield, turnEvent(turn.EventKindTurnCompleted))
		})
		fixture := newChatFixture(t, chatFixtureOptions{backend: script})
		issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
		w := newCaptureWriter()
		served := fixture.serveAsync(jsonPost(t, issued.Plaintext, defaultStreamBody()).WithContext(t.Context()), w)

		time.Sleep(10 * time.Second)
		step <- struct{}{}
		synctest.Wait()
		time.Sleep(14 * time.Second)
		synctest.Wait()
		_, body := w.snapshot()
		if frames := sseFrames(t, body); len(frames) != 1 || strings.Contains(body, frameKeepalives) {
			t.Fatalf("a chunk 14s ago must postpone the keepalive, got %q", frames)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		_, body = w.snapshot()
		if frames := sseFrames(t, body); len(frames) != 2 || frames[1] != frameKeepalives {
			t.Fatalf("frames = %q, want the content chunk then one keepalive", frames)
		}
		close(release)
		synctest.Wait()
		<-served
	})
}

func TestStreamClientDisconnectCancelsTheTurnAndWritesNothingMore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		observed := make(chan error, 1)
		script := scriptRuntime(func(ctx context.Context, yield func(store.RuntimeEvent, error) bool) {
			if !emit(yield, turnEvent(turn.EventKindTurnAccepted), answerEvent("first")) {
				return
			}
			<-ctx.Done()
			observed <- ctx.Err()
			emit(yield, turnEvent(turn.EventKindTurnCancelled))
		})
		fixture := newChatFixture(t, chatFixtureOptions{backend: script})
		issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
		w := newCaptureWriter()
		ctx, cancel := context.WithCancel(t.Context())
		served := fixture.serveAsync(jsonPost(t, issued.Plaintext, defaultStreamBody()).WithContext(ctx), w)
		synctest.Wait()
		_, before := w.snapshot()
		if got := contentOf(t, sseFrames(t, before)); got != "first" {
			t.Fatalf("streamed %q before the disconnect", got)
		}

		cancel()
		synctest.Wait()

		select {
		case <-served:
		default:
			t.Fatal("handler is still running after the client left")
		}
		select {
		case err := <-observed:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("runtime context error = %v, want cancellation", err)
			}
		default:
			t.Fatal("the turn never saw its context cancelled")
		}
		if _, after := w.snapshot(); after != before {
			t.Errorf("bytes written to a departed client: %q", strings.TrimPrefix(after, before))
		}
	})
}

func TestStreamWriteFailureCancelsTheTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		observed := make(chan error, 1)
		script := scriptRuntime(func(ctx context.Context, yield func(store.RuntimeEvent, error) bool) {
			if !emit(yield, turnEvent(turn.EventKindTurnAccepted), answerEvent("first")) {
				return
			}
			select {
			case <-ctx.Done():
				observed <- ctx.Err()
			case <-time.After(time.Hour):
				observed <- nil
			}
		})
		fixture := newChatFixture(t, chatFixtureOptions{backend: script})
		issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
		w := newCaptureWriter()
		w.failAfter = 0
		served := fixture.serveAsync(jsonPost(t, issued.Plaintext, defaultStreamBody()).WithContext(t.Context()), w)
		synctest.Wait()

		select {
		case <-served:
		default:
			t.Fatal("handler is still running after the write failed")
		}
		if err := <-observed; !errors.Is(err, context.Canceled) {
			t.Errorf("runtime context error = %v, want cancellation", err)
		}
		if _, body := w.snapshot(); body != "" {
			t.Errorf("body = %q, want nothing written", body)
		}
	})
}

func TestStreamArmsAWriteDeadlineForEveryWrite(t *testing.T) {
	fixture := newChatFixture(t, chatFixtureOptions{})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	w := newCaptureWriter()
	started := time.Now()

	<-fixture.serveAsync(jsonPost(t, issued.Plaintext, defaultStreamBody()).WithContext(t.Context()), w)

	deadlines := w.armedDeadlines()
	if len(deadlines) < 4 {
		t.Fatalf("deadlines armed = %d, want one for the headers and one per frame", len(deadlines))
	}
	for _, deadline := range deadlines {
		if deadline.Before(started.Add(writeDeadline)) || deadline.After(time.Now().Add(writeDeadline)) {
			t.Errorf("deadline %v is not now plus %v", deadline, writeDeadline)
		}
	}
}

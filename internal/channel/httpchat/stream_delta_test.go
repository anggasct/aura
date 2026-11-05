package httpchat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/anggasct/aura/internal/principal"
	"github.com/anggasct/aura/internal/runtime"
	"github.com/anggasct/aura/internal/store"
)

const (
	finishMaxTokens = "MAX_TOKENS"
	finishModelStop = "STOP"
)

func deltaEvent(index uint64, text string) store.RuntimeEvent {
	payload, err := runtime.EncodeModelDelta(runtime.ModelDelta{Text: text, Index: index})
	if err != nil {
		panic(err)
	}
	event := turnEvent(runtime.EventKindModelDelta)
	event.Payload = payload
	return event
}

func modelEvent(text, finish string, calls bool) store.RuntimeEvent {
	parts := []map[string]any{{"text": text}}
	if calls {
		parts = append(parts, map[string]any{"functionCall": map[string]any{"name": "web_search"}})
	}
	payload := map[string]any{"content": map[string]any{"role": "model", "parts": parts}}
	if finish != "" {
		payload["finishReason"] = finish
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	event := turnEvent(store.EventKindADK)
	event.Payload = raw
	return event
}

func toolResultEvent() store.RuntimeEvent {
	event := turnEvent(store.EventKindADK)
	event.Payload = json.RawMessage(`{"content":{"role":"user","parts":[{"functionResponse":{"name":"web_search"}}]}}`)
	return event
}

func modelAnswerTexts(events []store.RuntimeEvent) []string {
	var texts []string
	for i := range events {
		if events[i].Kind != store.EventKindADK {
			continue
		}
		var decoded struct {
			Content struct {
				Role  string `json:"role"`
				Parts []struct {
					Text         *string   `json:"text"`
					FunctionCall *struct{} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
			Partial bool `json:"partial"`
		}
		if err := json.Unmarshal(events[i].Payload, &decoded); err != nil || decoded.Partial || decoded.Content.Role != "model" {
			continue
		}
		var text strings.Builder
		answer := true
		for _, part := range decoded.Content.Parts {
			if part.FunctionCall != nil {
				answer = false
			}
			if part.Text != nil {
				text.WriteString(*part.Text)
			}
		}
		if answer && text.Len() > 0 {
			texts = append(texts, text.String())
		}
	}
	return texts
}

func contentChunks(t *testing.T, frames []string) []string {
	t.Helper()
	var chunks []string
	for _, frame := range frames {
		if frame == frameDoneMarker || !strings.HasPrefix(frame, frameDataMarker) {
			continue
		}
		decoded := decodeChunk(t, frame)
		for i := range decoded.Choices {
			if decoded.Choices[i].Delta.Content != "" {
				chunks = append(chunks, decoded.Choices[i].Delta.Content)
			}
		}
	}
	return chunks
}

func finishReasonOf(t *testing.T, frames []string) string {
	t.Helper()
	for _, frame := range frames {
		if frame == frameDoneMarker || !strings.HasPrefix(frame, frameDataMarker) {
			continue
		}
		decoded := decodeChunk(t, frame)
		for i := range decoded.Choices {
			if decoded.Choices[i].FinishReason != nil {
				return *decoded.Choices[i].FinishReason
			}
		}
	}
	return ""
}

func accepted() store.RuntimeEvent { return turnEvent(runtime.EventKindTurnAccepted) }

func completed() store.RuntimeEvent { return turnEvent(runtime.EventKindTurnCompleted) }

func TestStreamForwardsModelDeltasAsTheyArrive(t *testing.T) {
	cases := map[string]struct {
		streamed  []store.RuntimeEvent
		whole     []store.RuntimeEvent
		wantText  string
		wantWhole string
		wantParts []string
	}{
		"one message in token chunks": {
			streamed:  []store.RuntimeEvent{accepted(), deltaEvent(0, "Hel"), deltaEvent(1, "lo "), deltaEvent(2, "world"), modelEvent("Hello world", finishModelStop, false), completed()},
			whole:     []store.RuntimeEvent{accepted(), modelEvent("Hello world", finishModelStop, false), completed()},
			wantText:  "Hello world",
			wantWhole: "Hello world",
			wantParts: []string{"Hel", "lo ", "world"},
		},
		"completed message restores the unstreamed rest": {
			streamed:  []store.RuntimeEvent{accepted(), deltaEvent(0, "Hel"), modelEvent("Hello world", finishModelStop, false), completed()},
			whole:     []store.RuntimeEvent{accepted(), modelEvent("Hello world", finishModelStop, false), completed()},
			wantText:  "Hello world",
			wantWhole: "Hello world",
			wantParts: []string{"Hel", "lo world"},
		},
		"two messages around a tool call": {
			streamed: []store.RuntimeEvent{
				accepted(), deltaEvent(0, "on"), deltaEvent(1, "e"), modelEvent("one", finishModelStop, false),
				turnEvent(runtime.EventKindToolStarted), toolResultEvent(),
				deltaEvent(2, "tw"), deltaEvent(3, "o"), modelEvent("two", finishModelStop, false), completed()},
			whole: []store.RuntimeEvent{
				accepted(), modelEvent("one", finishModelStop, false), turnEvent(runtime.EventKindToolStarted), toolResultEvent(),
				modelEvent("two", finishModelStop, false), completed()},
			wantText:  "one\n\ntwo",
			wantWhole: "one\n\ntwo",
			wantParts: []string{"on", "e", "\n\ntw", "o"},
		},
		"text streamed before a tool call stays on the wire": {
			streamed: []store.RuntimeEvent{
				accepted(), deltaEvent(0, "Let me check."), modelEvent("Let me check.", "", true), toolResultEvent(),
				deltaEvent(1, "Done"), modelEvent("Done", finishModelStop, false), completed()},
			whole: []store.RuntimeEvent{
				accepted(), modelEvent("Let me check.", "", true), toolResultEvent(), modelEvent("Done", finishModelStop, false), completed()},
			wantText:  "Let me check.\n\nDone",
			wantWhole: "Done",
			wantParts: []string{"Let me check.", "\n\nDone"},
		},
		"message without deltas is sent whole": {
			streamed:  []store.RuntimeEvent{accepted(), modelEvent("whole", finishModelStop, false), completed()},
			whole:     []store.RuntimeEvent{accepted(), modelEvent("whole", finishModelStop, false), completed()},
			wantText:  "whole",
			wantWhole: "whole",
			wantParts: []string{"whole"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newChatFixture(t, chatFixtureOptions{backend: fixedTurn(tc.streamed...), answers: modelAnswerTexts})
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
			streamed := fixture.post(t, issued.Plaintext, defaultStreamBody())

			frames := sseFrames(t, streamed.Body.String())
			if got := contentChunks(t, frames); !slices.Equal(got, tc.wantParts) {
				t.Errorf("content chunks = %q, want %q", got, tc.wantParts)
			}
			if got := contentOf(t, frames); got != tc.wantText {
				t.Errorf("streamed text = %q, want %q", got, tc.wantText)
			}
			if frames[len(frames)-1] != frameDoneMarker || strings.Contains(streamed.Body.String(), `"error"`) {
				t.Errorf("stream did not end cleanly: %s", streamed.Body.String())
			}
			if first := decodeChunk(t, frames[0]); first.Choices[0].Delta.Role != "assistant" {
				t.Errorf("first chunk role = %q, want assistant", first.Choices[0].Delta.Role)
			}

			wholeFixture := newChatFixture(t, chatFixtureOptions{backend: fixedTurn(tc.whole...), answers: modelAnswerTexts})
			wholeKey := wholeFixture.keys.add(t, principal.ClassTrusted, testPersona)
			whole := decodeCompletion(t, wholeFixture.post(t, wholeKey.Plaintext, chatBody(userMessage("hi"))))
			if got := whole.Choices[0].Message.Content; got != tc.wantWhole {
				t.Errorf("non-streaming text = %q, want %q", got, tc.wantWhole)
			}
		})
	}
}

func TestStreamSurvivesDroppedModelDeltas(t *testing.T) {
	cases := map[string]struct {
		events    []store.RuntimeEvent
		wantParts []string
	}{
		"gap in the middle of a message": {
			events:    []store.RuntimeEvent{accepted(), deltaEvent(0, "Hel"), deltaEvent(2, "o w"), deltaEvent(3, "or"), modelEvent("Hello world", finishModelStop, false), completed()},
			wantParts: []string{"Hel", "lo world"},
		},
		"first delta lost": {
			events:    []store.RuntimeEvent{accepted(), deltaEvent(1, "llo"), modelEvent("Hello", finishModelStop, false), completed()},
			wantParts: []string{"Hello"},
		},
		"last delta lost": {
			events:    []store.RuntimeEvent{accepted(), deltaEvent(0, "Hel"), modelEvent("Hello", finishModelStop, false), completed()},
			wantParts: []string{"Hel", "lo"},
		},
		"restarted indexes with the same text are not forwarded twice": {
			events: []store.RuntimeEvent{
				accepted(), deltaEvent(0, "Hel"), deltaEvent(1, "lo"), deltaEvent(0, "Hel"), deltaEvent(1, "lo"),
				modelEvent("Hello", finishModelStop, false), completed()},
			wantParts: []string{"Hel", "lo"},
		},
		"next message streams again after the gap": {
			events: []store.RuntimeEvent{
				accepted(), deltaEvent(0, "on"), deltaEvent(2, "e"), modelEvent("one", finishModelStop, false),
				deltaEvent(3, "tw"), deltaEvent(4, "o"), modelEvent("two", finishModelStop, false), completed()},
			wantParts: []string{"on", "e", "\n\ntw", "o"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newChatFixture(t, chatFixtureOptions{backend: fixedTurn(tc.events...), answers: modelAnswerTexts})
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

			rec := fixture.post(t, issued.Plaintext, defaultStreamBody())

			frames := sseFrames(t, rec.Body.String())
			if frames[len(frames)-1] != frameDoneMarker || strings.Contains(rec.Body.String(), `"error"`) {
				t.Fatalf("a dropped delta broke the stream: %s", rec.Body.String())
			}
			want := strings.Join(modelAnswerTexts(tc.events), "\n\n")
			if got := contentOf(t, frames); got != want {
				t.Errorf("streamed text = %q, want %q", got, want)
			}
		})
	}
}

func TestStreamEndsWithOneErrorWhenTheTurnFailsAfterPartialText(t *testing.T) {
	cases := map[string]scriptRuntime{
		"failed event": fixedTurn(accepted(), deltaEvent(0, "par"), deltaEvent(1, "tial"), failedEvent("runtime_internal")),
		"iterator error": func(_ context.Context, yield func(store.RuntimeEvent, error) bool) {
			if emit(yield, accepted(), deltaEvent(0, "par"), deltaEvent(1, "tial")) {
				yield(store.RuntimeEvent{}, errors.New("boom with prompt text"))
			}
		},
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newChatFixture(t, chatFixtureOptions{backend: script, answers: modelAnswerTexts})
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

			rec := fixture.post(t, issued.Plaintext, defaultStreamBody())

			frames := sseFrames(t, rec.Body.String())
			if got := contentOf(t, frames[:len(frames)-1]); got != "partial" {
				t.Errorf("streamed %q before the failure, want partial", got)
			}
			failure := errorEventOf(t, frames[len(frames)-1])
			if failure.Error.Code != "internal_error" {
				t.Errorf("error event = %+v", failure.Error)
			}
			if strings.Count(rec.Body.String(), `"error"`) != 1 || strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), `"finish_reason":"`) {
				t.Errorf("failed stream was not one error event without a close: %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "boom") || strings.Contains(fixture.logs.String(), "boom") {
				t.Errorf("failure leaked the cause")
			}
		})
	}
}

func TestStreamDivergingFinalMessageEndsWithAnInternalError(t *testing.T) {
	events := []store.RuntimeEvent{accepted(), deltaEvent(0, "abc"), modelEvent("xyz", finishModelStop, false), completed()}
	fixture := newChatFixture(t, chatFixtureOptions{backend: fixedTurn(events...), answers: modelAnswerTexts})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

	rec := fixture.post(t, issued.Plaintext, defaultStreamBody())

	frames := sseFrames(t, rec.Body.String())
	failure := errorEventOf(t, frames[len(frames)-1])
	if failure.Error.Code != "internal_error" || strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("stream = %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "xyz") || strings.Contains(fixture.logs.String(), "xyz") || strings.Contains(fixture.logs.String(), "abc") {
		t.Errorf("model text leaked into the error or the logs")
	}
}

func TestFinishReasonReportsTruncationInBothModes(t *testing.T) {
	cases := map[string]struct {
		events []store.RuntimeEvent
		want   string
	}{
		"truncated answer": {
			events: []store.RuntimeEvent{accepted(), modelEvent("cut", finishMaxTokens, false), completed()},
			want:   "length",
		},
		"complete answer": {
			events: []store.RuntimeEvent{accepted(), modelEvent("done", finishModelStop, false), completed()},
			want:   "stop",
		},
		"provider without a finish reason": {
			events: []store.RuntimeEvent{accepted(), modelEvent("done", "", false), completed()},
			want:   "stop",
		},
		"only the last model message decides": {
			events: []store.RuntimeEvent{
				accepted(), modelEvent("cut", finishMaxTokens, true), toolResultEvent(),
				modelEvent("done", finishModelStop, false), completed()},
			want: "stop",
		},
		"truncated final message after a tool call": {
			events: []store.RuntimeEvent{
				accepted(), modelEvent("", finishModelStop, true), toolResultEvent(),
				modelEvent("cut", finishMaxTokens, false), completed()},
			want: "length",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newChatFixture(t, chatFixtureOptions{backend: fixedTurn(tc.events...), answers: modelAnswerTexts})
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

			whole := decodeCompletion(t, fixture.post(t, issued.Plaintext, chatBody(userMessage("hi"))))
			streamed := fixture.post(t, issued.Plaintext, defaultStreamBody())

			if got := whole.Choices[0].FinishReason; got != tc.want {
				t.Errorf("non-streaming finish_reason = %q, want %q", got, tc.want)
			}
			if got := finishReasonOf(t, sseFrames(t, streamed.Body.String())); got != tc.want {
				t.Errorf("streaming finish_reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStreamUsageIgnoresModelDeltas(t *testing.T) {
	final := modelEvent("Hello", finishModelStop, false)
	final.ProviderUsage = json.RawMessage(testUsageJSON)
	events := []store.RuntimeEvent{accepted(), deltaEvent(0, "Hel"), deltaEvent(1, "lo"), final, completed()}
	fixture := newChatFixture(t, chatFixtureOptions{backend: fixedTurn(events...), answers: modelAnswerTexts})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

	whole := decodeCompletion(t, fixture.post(t, issued.Plaintext, chatBody(userMessage("hi"))))
	streamed := fixture.post(t, issued.Plaintext, streamBody(true, userMessage("hi")))

	frames := sseFrames(t, streamed.Body.String())
	last := decodeChunk(t, frames[len(frames)-2])
	if last.Usage == nil || last.Usage.PromptTokens != 10 || last.Usage.CompletionTokens != 5 || last.Usage.TotalTokens != 15 {
		t.Fatalf("usage chunk = %+v, want 10/5/15", last.Usage)
	}
	if whole.Usage.TotalTokens != last.Usage.TotalTokens {
		t.Errorf("non-streaming usage %+v differs from streaming %+v", whole.Usage, last.Usage)
	}
}

func TestOnlyStreamingRequestsAskForPartialText(t *testing.T) {
	fixture := newChatFixture(t, chatFixtureOptions{})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

	decodeCompletion(t, fixture.post(t, issued.Plaintext, chatBody(userMessage("hi"))))
	fixture.post(t, issued.Plaintext, defaultStreamBody())

	turns := fixture.runtime.requests()
	if len(turns) != 2 || turns[0].StreamPartial || !turns[1].StreamPartial {
		t.Errorf("StreamPartial per request = %v, want [false true]", []bool{turns[0].StreamPartial, turns[1].StreamPartial})
	}
}

func TestSDKAccumulatesTokenLevelChunks(t *testing.T) {
	script := fixedTurn(
		accepted(), deltaEvent(0, "Hel"), deltaEvent(1, "lo "), deltaEvent(2, "world"),
		modelEvent("Hello world", finishMaxTokens, false), completed())
	fixture := newChatFixture(t, chatFixtureOptions{backend: script, answers: modelAnswerTexts})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	client := newSDKClient(server, issued.Plaintext)

	stream := client.Chat.Completions.NewStreaming(t.Context(), sdkParams(openai.UserMessage("hello")))
	defer func() { _ = stream.Close() }()
	accumulated := openai.ChatCompletionAccumulator{}
	var contents []string
	for stream.Next() {
		chunk := stream.Current()
		accumulated.AddChunk(chunk)
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				contents = append(contents, choice.Delta.Content)
			}
		}
	}

	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if !slices.Equal(contents, []string{"Hel", "lo ", "world"}) {
		t.Errorf("content chunks = %q", contents)
	}
	if len(accumulated.Choices) != 1 || accumulated.Choices[0].Message.Content != "Hello world" || accumulated.Choices[0].FinishReason != "length" {
		t.Errorf("accumulated = %+v", accumulated.Choices)
	}
}

func TestStreamFailsClosedWhenPartialTextIsResent(t *testing.T) {
	cases := map[string][]store.RuntimeEvent{
		"restarted indexes with different text": {
			accepted(), deltaEvent(0, "Hel"), deltaEvent(1, "lo"), deltaEvent(0, "Hey"), deltaEvent(1, "!"),
			modelEvent("Hey!", finishModelStop, false), completed()},
		"consecutive indexes repeating the text": {
			accepted(), deltaEvent(0, "Hel"), deltaEvent(1, "Hel"), modelEvent("Hello", finishModelStop, false), completed()},
	}
	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newChatFixture(t, chatFixtureOptions{backend: fixedTurn(events...), answers: modelAnswerTexts})
			issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)

			rec := fixture.post(t, issued.Plaintext, defaultStreamBody())

			frames := sseFrames(t, rec.Body.String())
			failure := errorEventOf(t, frames[len(frames)-1])
			if failure.Error.Code != "internal_error" || strings.Count(rec.Body.String(), `"error"`) != 1 {
				t.Errorf("stream = %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "[DONE]") || strings.Contains(rec.Body.String(), `"finish_reason":"`) {
				t.Errorf("a stream with resent text was closed as complete: %s", rec.Body.String())
			}
		})
	}
}

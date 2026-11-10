package httpchat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"

	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/store"
)

func newSDKClient(server *httptest.Server, key string, extra ...option.RequestOption) openai.Client {
	options := []option.RequestOption{
		option.WithBaseURL(server.URL + "/v1/"),
		option.WithAPIKey(key),
		option.WithHTTPClient(server.Client()),
		option.WithMaxRetries(0),
	}
	return openai.NewClient(append(options, extra...)...)
}

func TestSDKNonStreamingCompletionReturnsThePersonaReply(t *testing.T) {
	fixture := newChatFixture(t, chatFixtureOptions{maxMessages: 10, maxBytes: 1 << 16})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	client := newSDKClient(server, issued.Plaintext)
	var raw *http.Response

	completion, err := client.Chat.Completions.New(t.Context(), openai.ChatCompletionNewParams{
		Model: testPersona,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("be brief"),
			openai.UserMessage("hello"),
			openai.AssistantMessage("hi"),
			openai.UserMessage("how are you?"),
		},
	}, option.WithResponseInto(&raw))

	if err != nil {
		t.Fatalf("Completions.New: %v", err)
	}
	if completion.ID != "chatcmpl-"+testTurnID || completion.Model != testPersona || string(completion.Object) != "chat.completion" {
		t.Errorf("completion = %+v", completion)
	}
	if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != testReply ||
		completion.Choices[0].FinishReason != "stop" || string(completion.Choices[0].Message.Role) != "assistant" {
		t.Fatalf("choices = %+v", completion.Choices)
	}
	if completion.Usage.PromptTokens != 10 || completion.Usage.CompletionTokens != 5 || completion.Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", completion.Usage)
	}
	sessionID := raw.Header.Get(SessionHeader)
	if !strings.HasPrefix(sessionID, testPersona+"/http/"+issued.ID+"/") {
		t.Fatalf("session header = %q", sessionID)
	}
	turnRequest := fixture.runtime.requests()[0]
	if turnRequest.UntrustedContext == nil || len(turnRequest.UntrustedContext.Documents) != 3 || turnRequest.Parts[0].Text != "how are you?" {
		t.Errorf("turn = %+v context %+v", turnRequest.Parts, turnRequest.UntrustedContext)
	}

	continued, err := client.Chat.Completions.New(t.Context(), openai.ChatCompletionNewParams{
		Model:    testPersona,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("and now?")},
	}, option.WithHeader(SessionHeader, sessionID), option.WithResponseInto(&raw))
	if err != nil {
		t.Fatalf("continued Completions.New: %v", err)
	}
	if continued.Choices[0].Message.Content != testReply || raw.Header.Get(SessionHeader) != sessionID {
		t.Errorf("continued completion = %+v header %q", continued.Choices, raw.Header.Get(SessionHeader))
	}
	if turns := fixture.runtime.requests(); len(turns) != 2 || turns[1].SessionID != sessionID || turns[1].UntrustedContext != nil {
		t.Errorf("continued turn = %+v", turns)
	}
}

func TestSDKSurfacesTheDocumentedErrors(t *testing.T) {
	fixture := newChatFixture(t, chatFixtureOptions{})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	cases := map[string]struct {
		key      string
		model    string
		messages []openai.ChatCompletionMessageParamUnion
		status   int
		code     string
	}{
		"wrong persona":     {key: issued.Plaintext, model: "main", messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")}, status: http.StatusForbidden, code: "persona_not_permitted"},
		"invalid key":       {key: "aura_sk_bad", model: testPersona, messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("hi")}, status: http.StatusUnauthorized, code: "invalid_api_key"},
		"assistant is last": {key: issued.Plaintext, model: testPersona, messages: []openai.ChatCompletionMessageParamUnion{openai.AssistantMessage("hi")}, status: http.StatusBadRequest, code: "invalid_request"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client := newSDKClient(server, tc.key)

			_, err := client.Chat.Completions.New(t.Context(), openai.ChatCompletionNewParams{Model: tc.model, Messages: tc.messages})

			var apiErr *openai.Error
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status || apiErr.Code != tc.code {
				t.Fatalf("error = %v, want status %d code %s", err, tc.status, tc.code)
			}
		})
	}
}

func sdkParams(messages ...openai.ChatCompletionMessageParamUnion) openai.ChatCompletionNewParams {
	return openai.ChatCompletionNewParams{Model: testPersona, Messages: messages}
}

func TestSDKStreamingMatchesTheNonStreamingCompletion(t *testing.T) {
	script := fixedTurn(
		turnEvent(turn.EventKindTurnAccepted), answerEvent("first part"),
		turnEvent(turn.EventKindToolStarted), turnEvent(turn.EventKindToolCompleted),
		usageEvent("second part"), turnEvent(turn.EventKindTurnCompleted))
	fixture := newChatFixture(t, chatFixtureOptions{backend: script})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	client := newSDKClient(server, issued.Plaintext)
	whole, err := client.Chat.Completions.New(t.Context(), sdkParams(openai.UserMessage("hello")))
	if err != nil {
		t.Fatalf("Completions.New: %v", err)
	}
	var raw *http.Response
	params := sdkParams(openai.UserMessage("hello"))
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}

	stream := client.Chat.Completions.NewStreaming(t.Context(), params, option.WithResponseInto(&raw))
	defer func() { _ = stream.Close() }()
	accumulated := openai.ChatCompletionAccumulator{}
	var chunks []openai.ChatCompletionChunk
	for stream.Next() {
		chunk := stream.Current()
		chunks = append(chunks, chunk)
		accumulated.AddChunk(chunk)
	}

	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if got := raw.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("content type = %q", got)
	}
	if !strings.HasPrefix(raw.Header.Get(SessionHeader), testPersona+"/http/"+issued.ID+"/") {
		t.Errorf("session header = %q", raw.Header.Get(SessionHeader))
	}
	if len(accumulated.Choices) != 1 || accumulated.Choices[0].Message.Content != whole.Choices[0].Message.Content ||
		accumulated.Choices[0].Message.Content != "first part\n\nsecond part" {
		t.Fatalf("accumulated choices = %+v, non-streaming %q", accumulated.Choices, whole.Choices[0].Message.Content)
	}
	if string(accumulated.Choices[0].Message.Role) != "assistant" || accumulated.Choices[0].FinishReason != "stop" {
		t.Errorf("accumulated message = %+v finish %q", accumulated.Choices[0].Message, accumulated.Choices[0].FinishReason)
	}
	if len(chunks) < 3 || chunks[0].Choices[0].Delta.Role != "assistant" || chunks[0].ID != "chatcmpl-"+testTurnID || chunks[0].Model != testPersona {
		t.Fatalf("chunks = %+v", chunks)
	}
	last := chunks[len(chunks)-1]
	if len(last.Choices) != 0 || last.Usage.PromptTokens != whole.Usage.PromptTokens || last.Usage.CompletionTokens != whole.Usage.CompletionTokens || last.Usage.TotalTokens != 15 {
		t.Errorf("usage chunk = %+v, non-streaming usage %+v", last, whole.Usage)
	}
}

func TestSDKStreamingWithoutUsageEndsAfterTheFinishChunk(t *testing.T) {
	fixture := newChatFixture(t, chatFixtureOptions{})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	client := newSDKClient(server, issued.Plaintext)

	stream := client.Chat.Completions.NewStreaming(t.Context(), sdkParams(openai.UserMessage("hello")))
	defer func() { _ = stream.Close() }()
	var chunks []openai.ChatCompletionChunk
	for stream.Next() {
		chunks = append(chunks, stream.Current())
	}

	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(chunks) != 2 || chunks[0].Choices[0].Delta.Content != testReply || chunks[1].Choices[0].FinishReason != "stop" {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TestSDKStreamingSurfacesAMidStreamFailure(t *testing.T) {
	script := fixedTurn(turnEvent(turn.EventKindTurnAccepted), answerEvent("partial"), failedEvent("budget_exhausted"))
	fixture := newChatFixture(t, chatFixtureOptions{backend: script})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	client := newSDKClient(server, issued.Plaintext)

	stream := client.Chat.Completions.NewStreaming(t.Context(), sdkParams(openai.UserMessage("hello")))
	defer func() { _ = stream.Close() }()
	var content strings.Builder
	for stream.Next() {
		for _, choice := range stream.Current().Choices {
			content.WriteString(choice.Delta.Content)
		}
	}

	var streamErr *ssestream.StreamError
	if !errors.As(stream.Err(), &streamErr) || !strings.Contains(string(streamErr.Event.Data), `"code":"budget_exhausted"`) {
		t.Fatalf("stream error = %v, want the budget error event", stream.Err())
	}
	if content.String() != "partial" {
		t.Errorf("content before the failure = %q", content.String())
	}
}

func TestSDKStreamingGateErrorsArriveAsAPIErrors(t *testing.T) {
	fixture := newChatFixture(t, chatFixtureOptions{})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	client := newSDKClient(server, issued.Plaintext)
	params := sdkParams(openai.UserMessage("hello"))
	params.Model = "main"

	stream := client.Chat.Completions.NewStreaming(t.Context(), params)
	defer func() { _ = stream.Close() }()

	var apiErr *openai.Error
	if stream.Next() || !errors.As(stream.Err(), &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Code != "persona_not_permitted" {
		t.Fatalf("stream error = %v, want 403 persona_not_permitted", stream.Err())
	}
}

func TestSDKStreamingClientLeavingCancelsTheTurn(t *testing.T) {
	observed := make(chan error, 1)
	script := scriptRuntime(func(ctx context.Context, yield func(store.RuntimeEvent, error) bool) {
		if !emit(yield, turnEvent(turn.EventKindTurnAccepted), answerEvent("first")) {
			return
		}
		<-ctx.Done()
		observed <- ctx.Err()
	})
	fixture := newChatFixture(t, chatFixtureOptions{backend: script})
	issued := fixture.keys.add(t, principal.ClassTrusted, testPersona)
	server := httptest.NewServer(fixture.handler)
	defer server.Close()
	client := newSDKClient(server, issued.Plaintext)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream := client.Chat.Completions.NewStreaming(ctx, sdkParams(openai.UserMessage("hello")))
	defer func() { _ = stream.Close() }()
	if !stream.Next() || stream.Current().Choices[0].Delta.Content != "first" {
		t.Fatalf("first chunk missing: %v", stream.Err())
	}
	cancel()

	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("runtime context error = %v, want cancellation", err)
		}
	case <-time.After(testWait):
		t.Fatal("the turn kept running after the client left")
	}
}

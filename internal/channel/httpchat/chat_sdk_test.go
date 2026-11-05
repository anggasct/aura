package httpchat

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/anggasct/aura/internal/principal"
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
	turn := fixture.runtime.requests()[0]
	if turn.UntrustedContext == nil || len(turn.UntrustedContext.Documents) != 3 || turn.Parts[0].Text != "how are you?" {
		t.Errorf("turn = %+v context %+v", turn.Parts, turn.UntrustedContext)
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

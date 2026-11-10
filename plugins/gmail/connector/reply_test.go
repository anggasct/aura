package connector

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/plugin/wire"
)

type originalMail struct {
	id       string
	threadID string
	headers  []map[string]string
}

func replyAPI(original *originalMail) *fakeAPI {
	return newAPI(func(req *wire.Request) (*wire.Response, error) {
		if req.Method == http.MethodGet {
			return jsonResponse(200, map[string]any{
				"id": original.id, "threadId": original.threadID,
				"payload": map[string]any{"headers": original.headers},
			}), nil
		}
		return jsonResponse(200, map[string]any{"id": "m9", "threadId": original.threadID}), nil
	})
}

func replyArguments(subject string) map[string]any {
	return with(with(baseArguments(), "subject", subject), "reply_to_id", "orig1")
}

func goodOriginal() *originalMail {
	return &originalMail{
		id: "orig1", threadID: "thread1",
		headers: headersOf("Subject", "Hello", "Message-ID", "<abc123@mail.example.com>", "References", "<root@mail.example.com> <mid@mail.example.com>"),
	}
}

func TestReplyJoinsTheThread(t *testing.T) {
	cases := []struct {
		name    string
		subject string
	}{
		{"same subject", "Hello"},
		{"added prefix", "Re: Hello"},
		{"upper case prefix", "RE: re: HELLO"},
		{"extra spaces", "Re:   Hello "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := replyAPI(goodOriginal())
			if _, err := invoke(t, api, "send", replyArguments(strings.TrimSpace(tc.subject))); err != nil {
				t.Fatalf("send: %v", err)
			}
			requests := api.seen()
			if len(requests) != 2 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodPost {
				t.Fatalf("requests = %+v", requests)
			}
			query := queryOf(t, requests[0].URL)
			if query.Get("format") != "metadata" || strings.Join(query["metadataHeaders"], ",") != "Message-ID,References,Subject" {
				t.Fatalf("original lookup = %s", requests[0].URL)
			}
			sent := decodeSent(t, &requests[1])
			header := sent.message.Header
			if sent.threadID != "thread1" || header.Get("In-Reply-To") != "<abc123@mail.example.com>" {
				t.Fatalf("thread = %q, In-Reply-To = %q", sent.threadID, header.Get("In-Reply-To"))
			}
			want := "<root@mail.example.com> <mid@mail.example.com> <abc123@mail.example.com>"
			if got := strings.Join(strings.Fields(header.Get("References")), " "); got != want {
				t.Fatalf("References = %q, want %q", got, want)
			}
		})
	}
}

func TestReplyToAMessageWithoutReferencesStartsTheChain(t *testing.T) {
	original := goodOriginal()
	original.headers = headersOf("Subject", "Hello", "Message-ID", "<abc123@mail.example.com>")
	api := replyAPI(original)
	if _, err := invoke(t, api, "draft", replyArguments("Hello")); err != nil {
		t.Fatalf("draft: %v", err)
	}
	sent := decodeSent(t, &api.posts()[0])
	if sent.message.Header.Get("References") != "<abc123@mail.example.com>" || sent.threadID != "thread1" {
		t.Fatalf("References = %q, thread = %q", sent.message.Header.Get("References"), sent.threadID)
	}
}

func TestReplyReferencesAreBounded(t *testing.T) {
	var chain []string
	for i := range 40 {
		chain = append(chain, fmt.Sprintf("<ref%d@mail.example.com>", i))
	}
	original := goodOriginal()
	original.headers = headersOf("Subject", "Hello", "Message-ID", "<abc123@mail.example.com>", "References", strings.Join(chain, " "))
	api := replyAPI(original)
	if _, err := invoke(t, api, "send", replyArguments("Hello")); err != nil {
		t.Fatalf("send: %v", err)
	}
	references := strings.Fields(decodeSent(t, &api.posts()[0]).message.Header.Get("References"))
	if len(references) != maxReferences+1 || references[0] != "<ref30@mail.example.com>" || references[len(references)-1] != "<abc123@mail.example.com>" {
		t.Fatalf("References = %v", references)
	}
}

func TestReplyDropsReferencesThatAreNotMessageIDs(t *testing.T) {
	original := goodOriginal()
	original.headers = headersOf("Subject", "Hello", "Message-ID", "<abc123@mail.example.com>", "References", "<ok@mail.example.com> garbage (comment) <bad id@x> <>")
	api := replyAPI(original)
	if _, err := invoke(t, api, "send", replyArguments("Hello")); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := strings.Fields(decodeSent(t, &api.posts()[0]).message.Header.Get("References"))
	if len(got) != 2 || got[0] != "<ok@mail.example.com>" {
		t.Fatalf("References = %v", got)
	}
}

func TestReplyThatCannotBeThreadedFailsBeforeAnyMutatingRequest(t *testing.T) {
	cases := []struct {
		name     string
		subject  string
		headers  []map[string]string
		thread   string
		want     Code
		approved string
	}{
		{"changed subject", "Hello", headersOf("Subject", "Quarterly numbers", "Message-ID", "<abc@mail.example.com>"), "thread1", CodeSubjectMismatch, "Hello"},
		{"changed subject after the prefix", "Hello", headersOf("Subject", "Re: Hello world", "Message-ID", "<abc@mail.example.com>"), "thread1", CodeSubjectMismatch, "Re: Hello"},
		{"original without a subject", "Hello", headersOf("Message-ID", "<abc@mail.example.com>"), "thread1", CodeSubjectMismatch, "Hello"},
		{"message id with a crlf", "Hello", headersOf("Subject", "Hello", "Message-ID", "<abc@mail.example.com>\r\nBcc: eve@example.com"), "thread1", CodeReplyUnavailable, "Hello"},
		{"message id with a bare lf", "Hello", headersOf("Subject", "Hello", "Message-ID", "<abc@mail.example.com>\nX: y"), "thread1", CodeReplyUnavailable, "Hello"},
		{"message id with a nul", "Hello", headersOf("Subject", "Hello", "Message-ID", "<abc\x00@mail.example.com>"), "thread1", CodeReplyUnavailable, "Hello"},
		{"message id with a space", "Hello", headersOf("Subject", "Hello", "Message-ID", "<a bc@mail.example.com>"), "thread1", CodeReplyUnavailable, "Hello"},
		{"message id without brackets", "Hello", headersOf("Subject", "Hello", "Message-ID", "abc@mail.example.com"), "thread1", CodeReplyUnavailable, "Hello"},
		{"missing message id", "Hello", headersOf("Subject", "Hello"), "thread1", CodeReplyUnavailable, "Hello"},
		{"two message ids", "Hello", headersOf("Subject", "Hello", "Message-ID", "<a@mail.example.com>", "Message-ID", "<b@mail.example.com>"), "thread1", CodeReplyUnavailable, "Hello"},
		{"references with a control character", "Hello", headersOf("Subject", "Hello", "Message-ID", "<a@mail.example.com>", "References", "<r@mail.example.com>\r\nBcc: eve@example.com"), "thread1", CodeReplyUnavailable, "Hello"},
		{"thread id that is a path", "Hello", headersOf("Subject", "Hello", "Message-ID", "<a@mail.example.com>"), "../x", CodeResponseInvalid, "Hello"},
		{"empty thread id", "Hello", headersOf("Subject", "Hello", "Message-ID", "<a@mail.example.com>"), "", CodeResponseInvalid, "Hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := replyAPI(&originalMail{id: "orig1", threadID: tc.thread, headers: tc.headers})
			_, err := invoke(t, api, "send", replyArguments(tc.approved))
			mustCode(t, err, tc.want)
			if len(api.posts()) != 0 {
				t.Fatalf("a mutating request was made: %+v", api.posts())
			}
		})
	}
}

func TestReplyWhoseLookupReturnsADifferentIDIsRefused(t *testing.T) {
	api := replyAPI(&originalMail{id: "someone-else", threadID: "t", headers: headersOf("Subject", "Hello", "Message-ID", "<a@mail.example.com>")})
	_, err := invoke(t, api, "send", replyArguments("Hello"))
	mustCode(t, err, CodeResponseInvalid)
	if len(api.posts()) != 0 {
		t.Fatalf("posts = %+v", api.posts())
	}
}

func TestReplyToAMissingMessageFailsAsNotFound(t *testing.T) {
	api := newAPI(func(*wire.Request) (*wire.Response, error) { return statusOnly(404), nil })
	_, err := invoke(t, api, "send", replyArguments("Hello"))
	mustCode(t, err, CodeNotFound)
	if len(api.posts()) != 0 {
		t.Fatalf("posts = %+v", api.posts())
	}
}

func TestReplyDecodesAnEncodedOriginalSubject(t *testing.T) {
	original := goodOriginal()
	original.headers = headersOf("Subject", "=?UTF-8?B?Q2Fmw6k=?=", "Message-ID", "<abc123@mail.example.com>")
	api := replyAPI(original)
	if _, err := invoke(t, api, "send", replyArguments("Café")); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestOnlyTheApprovedSubjectIsSent(t *testing.T) {
	original := goodOriginal()
	original.headers = headersOf("Subject", "RE: hello", "Message-ID", "<abc123@mail.example.com>")
	api := replyAPI(original)
	if _, err := invoke(t, api, "send", replyArguments("Hello")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := decodeSent(t, &api.posts()[0]).message.Header.Get("Subject"); got != "Hello" {
		t.Fatalf("Subject = %q, want the approved text", got)
	}
}

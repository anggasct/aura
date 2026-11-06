package gmail

import (
	"context"
	"encoding/json"
	"net/url"
)

type readArguments struct {
	ID string `json:"id"`
}

type readHeaders struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Cc        string `json:"cc"`
	Subject   string `json:"subject"`
	Date      string `json:"date"`
	MessageID string `json:"message_id"`
}

type readOutput struct {
	ID          string       `json:"id"`
	ThreadID    string       `json:"thread_id"`
	Headers     readHeaders  `json:"headers"`
	Body        string       `json:"body"`
	Truncated   bool         `json:"truncated"`
	Attachments []attachment `json:"attachments"`
}

func read(ctx context.Context, api *client, raw json.RawMessage) (json.RawMessage, error) {
	var args readArguments
	if err := decodeArguments(raw, &args, "id"); err != nil {
		return nil, err
	}
	if !validID(args.ID) {
		return nil, fail(CodeInvalidArgument)
	}
	body, err := api.get(ctx, messagePath(args.ID), url.Values{"format": {"full"}})
	if err != nil {
		return nil, err
	}
	fetched, err := decodeMessage(body)
	if err != nil {
		return nil, err
	}
	if fetched.ID != args.ID || !validID(fetched.ThreadID) || fetched.Payload == nil {
		return nil, fail(CodeResponseInvalid)
	}
	extracted := extractBody(fetched.Payload)
	headers := fetched.Payload.Headers
	output := readOutput{
		ID:       fetched.ID,
		ThreadID: fetched.ThreadID,
		Headers: readHeaders{
			From:      headerText(headerValue(headers, "From"), maxHeaderRunes),
			To:        headerText(headerValue(headers, "To"), maxListRunes),
			Cc:        headerText(headerValue(headers, "Cc"), maxListRunes),
			Subject:   headerText(headerValue(headers, "Subject"), maxHeaderRunes),
			Date:      headerText(headerValue(headers, "Date"), maxHeaderRunes),
			MessageID: headerText(headerValue(headers, "Message-ID"), maxHeaderRunes),
		},
		Body:        extracted.text,
		Truncated:   extracted.truncated,
		Attachments: extracted.attachments,
	}
	if output.Attachments == nil {
		output.Attachments = []attachment{}
	}
	return marshalOutput(&output)
}

package gmail

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

type searchArguments struct {
	Query      *string `json:"query"`
	MaxResults *int    `json:"max_results"`
}

type listResponse struct {
	Messages []struct {
		ID       string `json:"id"`
		ThreadID string `json:"threadId"`
	} `json:"messages"`
}

type summary struct {
	ID       string `json:"id"`
	ThreadID string `json:"thread_id"`
	From     string `json:"from"`
	Subject  string `json:"subject"`
	Date     string `json:"date"`
	Snippet  string `json:"snippet"`
}

type searchOutput struct {
	Messages []summary `json:"messages"`
}

func search(ctx context.Context, api *client, raw json.RawMessage) (json.RawMessage, error) {
	var args searchArguments
	if err := decodeArguments(raw, &args, "query", "max_results"); err != nil {
		return nil, err
	}
	if args.Query == nil {
		return nil, fail(CodeInvalidArgument)
	}
	text := *args.Query
	limit := defaultResults
	if args.MaxResults != nil {
		limit = *args.MaxResults
	}
	if limit < 1 || limit > maxResults || len(text) > maxQueryBytes || !utf8.ValidString(text) {
		return nil, fail(CodeInvalidArgument)
	}
	query := url.Values{"maxResults": {strconv.Itoa(limit)}}
	if strings.TrimSpace(text) != "" {
		query.Set("q", text)
	}
	body, err := api.get(ctx, "messages", query)
	if err != nil {
		return nil, err
	}
	var listed listResponse
	if err := json.Unmarshal(body, &listed); err != nil || len(listed.Messages) > limit {
		return nil, fail(CodeResponseInvalid)
	}
	for _, ref := range listed.Messages {
		if !validID(ref.ID) {
			return nil, fail(CodeResponseInvalid)
		}
	}
	output := searchOutput{Messages: make([]summary, 0, len(listed.Messages))}
	for _, ref := range listed.Messages {
		found, err := summarize(ctx, api, ref.ID)
		if err != nil {
			if code, ok := codeOf(err); ok && code == CodeNotFound {
				continue
			}
			return nil, err
		}
		output.Messages = append(output.Messages, *found)
	}
	return marshalOutput(&output)
}

func summarize(ctx context.Context, api *client, id string) (*summary, error) {
	query := url.Values{"format": {"metadata"}, "metadataHeaders": {"From", "Subject", "Date"}}
	body, err := api.get(ctx, messagePath(id), query)
	if err != nil {
		return nil, err
	}
	fetched, err := decodeMessage(body)
	if err != nil {
		return nil, err
	}
	if !validID(fetched.ThreadID) {
		return nil, fail(CodeResponseInvalid)
	}
	var headers []header
	if fetched.Payload != nil {
		headers = fetched.Payload.Headers
	}
	return &summary{
		ID:       id,
		ThreadID: fetched.ThreadID,
		From:     headerText(headerValue(headers, "From"), maxHeaderRunes),
		Subject:  headerText(headerValue(headers, "Subject"), maxHeaderRunes),
		Date:     headerText(headerValue(headers, "Date"), maxHeaderRunes),
		Snippet:  headerText(html.UnescapeString(fetched.Snippet), maxSnippetRunes),
	}, nil
}

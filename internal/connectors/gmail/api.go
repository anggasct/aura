package gmail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/anggasct/aura/internal/connectors/wire"
)

const (
	apiBase        = "https://gmail.googleapis.com/gmail/v1/users/me/"
	failureTooBig  = "response_too_large"
	failureEgress  = "egress_denied"
	contentTypeKey = "Content-Type"
)

type client struct {
	do wire.Do
}

func (c *client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	target := apiBase + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	return c.send(ctx, &wire.Request{Method: http.MethodGet, URL: target, Headers: map[string]string{"Accept": "application/json"}})
}

func (c *client) post(ctx context.Context, path string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fail(CodeInvalidArgument)
	}
	return c.send(ctx, &wire.Request{
		Method:  http.MethodPost,
		URL:     apiBase + path,
		Headers: map[string]string{"Accept": "application/json", contentTypeKey: "application/json"},
		Body:    body,
	})
}

func (c *client) send(ctx context.Context, req *wire.Request) ([]byte, error) {
	response, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, fail(CodeUpstream)
	}
	if response.Failure != "" || response.Status == 0 {
		return nil, failureError(response.Failure)
	}
	if response.Status < 200 || response.Status > 299 {
		return nil, statusError(response.Status)
	}
	return response.Body, nil
}

func failureError(failure string) error {
	switch failure {
	case failureTooBig:
		return fail(CodeMessageTooLarge)
	case failureEgress:
		return fail(CodeEgressDenied)
	}
	return fail(CodeRequestFailed)
}

func statusError(status int) error {
	switch status {
	case http.StatusUnauthorized:
		return fail(CodeUnauthorized)
	case http.StatusForbidden:
		return fail(CodeForbidden)
	case http.StatusNotFound:
		return fail(CodeNotFound)
	case http.StatusTooManyRequests:
		return fail(CodeRateLimited)
	case http.StatusBadRequest, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return fail(CodeRequestRejected)
	}
	return fail(CodeUpstream)
}

func messagePath(id string) string {
	return "messages/" + id
}

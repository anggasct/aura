package connector

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/anggasct/aura/internal/egress"
)

const (
	httpErrorRequestFailed    = "request_failed"
	httpErrorResponseTooLarge = "response_too_large"
	userAgent                 = "aura-connector"
)

var (
	safeMethods    = []string{http.MethodGet, http.MethodHead}
	allowedMethods = []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}
	refusedStatus  = []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity, http.StatusTooManyRequests}
	returnedHeader = []string{"Content-Type", "Retry-After", "ETag"}
	droppedHeaders = []string{
		"Authorization", "Cookie", "Host", "Connection", "Keep-Alive", "Te", "Trailer",
		"Transfer-Encoding", "Upgrade", "Content-Length", "Expect", "User-Agent",
	}
)

func httpDenied(call string) *httpResultMessage {
	return &httpResultMessage{V: Version, Type: typeHTTPResult, Call: call, Error: string(egress.ErrorCodeEgressDenied)}
}

func (h *host) approvedHost(target *url.URL) bool {
	port := target.Port()
	if port == "" {
		port = "443"
	}
	return h.allowed[strings.ToLower(target.Hostname())+":"+port]
}

func (h *host) credentialHost(target *url.URL) bool {
	port := target.Port()
	if port == "" {
		port = "443"
	}
	return h.credited[strings.ToLower(target.Hostname())+":"+port]
}

func requestHeaders(supplied map[string]string) (http.Header, *Error) {
	if len(supplied) > maxHeaders {
		return nil, invalid()
	}
	headers := make(http.Header, len(supplied))
	for name, value := range supplied {
		if !headerNamePattern.MatchString(name) || !validHeaderValue(value) {
			return nil, invalid()
		}
		canonical := http.CanonicalHeaderKey(name)
		if _, repeated := headers[canonical]; repeated {
			return nil, invalid()
		}
		if slices.Contains(droppedHeaders, canonical) || strings.HasPrefix(canonical, "Proxy-") {
			continue
		}
		headers.Set(canonical, value)
	}
	headers.Set("User-Agent", userAgent)
	return headers, nil
}

func (h *host) perform(callCtx, ctx context.Context, tool *Tool, state *callState, message *httpMessage) (*httpResultMessage, *Error) {
	if !slices.Contains(allowedMethods, message.Method) || message.URL == "" || len(message.URL) > maxURLBytes {
		return nil, invalid()
	}
	mutating := !slices.Contains(safeMethods, message.Method)
	if !tool.Effect && mutating {
		return nil, Errorf(ErrTierViolation, "a read tool asked for a request that can change state")
	}
	body, fail := decodeBody(message.BodyB64)
	if fail != nil {
		return nil, fail
	}
	headers, fail := requestHeaders(message.Headers)
	if fail != nil {
		return nil, fail
	}
	target, err := url.Parse(message.URL)
	if err != nil {
		return nil, invalid()
	}
	if target.Scheme != "https" || target.Host == "" || target.User != nil || target.Fragment != "" || !h.approvedHost(target) {
		return httpDenied(state.id), nil
	}
	request, err := http.NewRequestWithContext(callCtx, message.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, invalid()
	}
	request.Header = headers
	if h.credentialHost(target) {
		if err := h.cfg.Auth.Authorize(callCtx, request); err != nil {
			return nil, authFailure(err)
		}
	}
	response, err := h.client.Do(request)
	if err != nil {
		return h.transportFailure(ctx, callCtx, state, mutating, err)
	}
	defer func() { _ = response.Body.Close() }()
	if mutating {
		if fail := classifyMutation(state, response.StatusCode); fail != nil {
			return nil, fail
		}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes+1))
	switch {
	case err != nil && mutating:
		if fail := h.interrupted(ctx, callCtx); fail != nil {
			return nil, fail
		}
		data = nil
	case err != nil:
		return h.transportFailure(ctx, callCtx, state, mutating, err)
	case len(data) > maxBodyBytes && mutating:
		data = nil
	case len(data) > maxBodyBytes:
		return &httpResultMessage{V: Version, Type: typeHTTPResult, Call: state.id, Error: httpErrorResponseTooLarge}, nil
	}
	result := &httpResultMessage{V: Version, Type: typeHTTPResult, Call: state.id, Status: response.StatusCode, BodyB64: base64.StdEncoding.EncodeToString(data)}
	for _, name := range returnedHeader {
		if value := response.Header.Get(name); value != "" && validHeaderValue(value) {
			if result.Headers == nil {
				result.Headers = make(map[string]string, len(returnedHeader))
			}
			result.Headers[name] = value
		}
	}
	return result, nil
}

func classifyMutation(state *callState, status int) *Error {
	switch {
	case status >= 200 && status < 300:
		state.sent = true
		state.lastMutation = mutationAccepted
		return nil
	case slices.Contains(refusedStatus, status) && !state.sent:
		state.lastMutation = mutationRefused
		return nil
	}
	state.sent = true
	return Errorf(ErrRequestFailed, "the outcome of the request cannot be confirmed")
}

func (h *host) interrupted(ctx, callCtx context.Context) *Error {
	switch {
	case ctx.Err() != nil:
		return Wrap(ErrBackendUnavailable, ctx.Err(), "the call was cancelled")
	case callCtx.Err() != nil:
		return Errorf(ErrRPCLimit, "the call exceeded its time limit")
	}
	return nil
}

func (h *host) transportFailure(ctx, callCtx context.Context, state *callState, mutating bool, err error) (*httpResultMessage, *Error) {
	code, denied := egress.CodeOf(err)
	denied = denied && code == egress.ErrorCodeEgressDenied
	if mutating && !denied && !neverReachedTheServer(err) {
		state.sent = true
	}
	if fail := h.interrupted(ctx, callCtx); fail != nil {
		return nil, fail
	}
	switch {
	case denied:
		return httpDenied(state.id), nil
	case !mutating:
		return &httpResultMessage{V: Version, Type: typeHTTPResult, Call: state.id, Error: httpErrorRequestFailed}, nil
	}
	return nil, Errorf(ErrRequestFailed, "the request could not be completed")
}

func neverReachedTheServer(err error) bool {
	var dial *net.OpError
	if errors.As(err, &dial) && dial.Op == "dial" {
		return true
	}
	var verification *tls.CertificateVerificationError
	return errors.As(err, &verification)
}

func authFailure(err error) *Error {
	code, coded := authCode(err)
	if !coded {
		return Errorf(ErrAuthUnavailable, "the plugin authorization is unavailable")
	}
	return Errorf(code, "the plugin authorization is not usable")
}

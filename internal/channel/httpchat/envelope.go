package httpchat

import (
	"encoding/json"
	"net/http"
)

const (
	errorTypeInvalidRequest = "invalid_request_error"
	errorTypeAuthentication = "authentication_error"
	errorTypeRateLimit      = "rate_limit_error"
	errorTypeServer         = "server_error"
)

type errorSpec struct {
	status  int
	kind    string
	code    string
	message string
}

var (
	errNotFound = errorSpec{
		status: http.StatusNotFound, kind: errorTypeInvalidRequest, code: "not_found",
		message: "The requested route does not exist.",
	}
	errMethodNotAllowed = errorSpec{
		status: http.StatusMethodNotAllowed, kind: errorTypeInvalidRequest, code: "method_not_allowed",
		message: "The method is not allowed for this route.",
	}
	errInvalidAPIKey = errorSpec{
		status: http.StatusUnauthorized, kind: errorTypeAuthentication, code: "invalid_api_key",
		message: "The API key is missing or invalid.",
	}
	errRuntimeOverloaded = errorSpec{
		status: http.StatusServiceUnavailable, kind: errorTypeServer, code: "runtime_overloaded",
		message: "Too many requests are in flight for this API key.",
	}
	errRateLimited = errorSpec{
		status: http.StatusTooManyRequests, kind: errorTypeRateLimit, code: "rate_limit_exceeded",
		message: "The request rate for this API key is exceeded.",
	}
	errUnsupportedMediaType = errorSpec{
		status: http.StatusUnsupportedMediaType, kind: errorTypeInvalidRequest, code: "unsupported_media_type",
		message: "The request content type must be application/json.",
	}
	errRequestTooLarge = errorSpec{
		status: http.StatusRequestEntityTooLarge, kind: errorTypeInvalidRequest, code: "request_too_large",
		message: "The request body is too large.",
	}
	errInvalidRequest = errorSpec{
		status: http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request",
		message: "The request could not be read.",
	}
	errInternal = errorSpec{
		status: http.StatusInternalServerError, kind: errorTypeServer, code: "internal_error",
		message: "The server could not complete the request.",
	}
)

type errorEnvelope struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

func writeError(w http.ResponseWriter, spec *errorSpec) {
	writeJSON(w, spec.status, &errorEnvelope{Error: errorDetail{Message: spec.message, Type: spec.kind, Code: spec.code}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

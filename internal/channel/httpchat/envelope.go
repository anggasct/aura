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

const (
	errorTypePermission   = "permission_error"
	errorTypeInsufficient = "insufficient_quota"
	paramMessages         = "messages"
	paramModel            = "model"
	paramN                = "n"
	paramSessionHeader    = "X-Aura-Session"
)

type errorSpec struct {
	status  int
	kind    string
	code    string
	message string
	param   string
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
	errBodyNotJSON = errorSpec{
		status: http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request",
		message: "The request body must be a single JSON object with the documented field types.",
	}
	errMessagesRequired = errorSpec{
		status: http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request",
		message: "The messages field must contain at least one message.", param: paramMessages,
	}
	errMessageShape = errorSpec{
		status: http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request",
		message: "Each message needs a system, user or assistant role and string or text-part content.", param: paramMessages,
	}
	errLastMessageNotUser = errorSpec{
		status: http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request",
		message: "The last message must have the user role and a non-empty text.", param: paramMessages,
	}
	errUnsupportedContent = errorSpec{
		status: http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "unsupported_content",
		message: "Only text content parts are supported.", param: paramMessages,
	}
	errChoiceCount = errorSpec{
		status: http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request",
		message: "Only n=1 is supported.", param: paramN,
	}
	errSessionHeader = errorSpec{
		status: http.StatusBadRequest, kind: errorTypeInvalidRequest, code: "invalid_request",
		message: "The X-Aura-Session header must be sent at most once.", param: paramSessionHeader,
	}
	errPersonaNotPermitted = errorSpec{
		status: http.StatusForbidden, kind: errorTypePermission, code: "persona_not_permitted",
		message: "The model does not match the persona this API key is bound to.", param: paramModel,
	}
	errAudienceDenied = errorSpec{
		status: http.StatusForbidden, kind: errorTypePermission, code: "audience_denied",
		message: "The persona does not serve this API key's access class.",
	}
	errEvalGateClosed = errorSpec{
		status: http.StatusForbidden, kind: errorTypePermission, code: "eval_gate_closed",
		message: "The persona is not available to this API key yet.",
	}
	errSessionNotFound = errorSpec{
		status: http.StatusNotFound, kind: errorTypeInvalidRequest, code: "session_not_found",
		message: "The session does not exist for this API key.", param: paramSessionHeader,
	}
	errHandoffOpen = errorSpec{
		status: http.StatusConflict, kind: errorTypeInvalidRequest, code: "handoff_open",
		message: "The session is handed over to the owner.",
	}
	errBudgetExhausted = errorSpec{
		status: http.StatusTooManyRequests, kind: errorTypeInsufficient, code: "budget_exhausted",
		message: "The usage budget for this API key is exhausted.",
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

func newErrorEnvelope(spec *errorSpec) *errorEnvelope {
	detail := errorDetail{Message: spec.message, Type: spec.kind, Code: spec.code}
	if spec.param != "" {
		param := spec.param
		detail.Param = &param
	}
	return &errorEnvelope{Error: detail}
}

func writeError(w http.ResponseWriter, spec *errorSpec) {
	writeJSON(w, spec.status, newErrorEnvelope(spec))
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

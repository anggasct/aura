package httpchat

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/channels/kit/access"
	"github.com/anggasct/aura/internal/channels/kit/approval"
	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/agent"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/internal/platform/turn"
	"github.com/anggasct/aura/internal/platform/turn/ingress"
	"github.com/anggasct/aura/internal/store"
)

const (
	SessionHeader = "X-Aura-Session"

	sessionSource      = "http"
	sessionTokenLength = 26
	sessionTokenBytes  = 16
	rateRetryAfter     = "60"
	answerSeparator    = "\n\n"
	finishStop         = "stop"
	finishLength       = "length"
)

type Session struct {
	ID        string
	OwnerID   string
	PersonaID string
}

type SessionStore interface {
	Get(ctx context.Context, id string) (Session, bool, error)
	Create(ctx context.Context, session *Session) error
}

type Personas interface {
	Lookup(id string) (agent.Definition, bool)
}

type ChatOptions struct {
	Runtime            turn.AgentRuntime
	Sessions           SessionStore
	Personas           Personas
	Budgets            access.BudgetGate
	Evals              access.EvalGate
	Handoffs           access.HandoffGate
	Approvals          *approval.OwnerRoute
	Denied             func(ctx context.Context, reason string)
	AnswerTexts        func(events []store.RuntimeEvent) []string
	MaxHistoryMessages int
	MaxHistoryBytes    int
	KeepaliveInterval  time.Duration
	Logger             *slog.Logger
}

type Chat struct {
	runtime     turn.AgentRuntime
	sessions    SessionStore
	personas    Personas
	budgets     access.BudgetGate
	evals       access.EvalGate
	handoffs    access.HandoffGate
	approvals   *approval.OwnerRoute
	denied      func(ctx context.Context, reason string)
	answerTexts func(events []store.RuntimeEvent) []string
	maxMessages int
	maxBytes    int
	keepalive   time.Duration
	logger      *slog.Logger
}

var _ ChatHandler = (*Chat)(nil)

func NewChat(opts *ChatOptions) (*Chat, error) {
	switch {
	case opts == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "chat options must not be nil")
	case opts.Runtime == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "runtime must not be nil")
	case opts.Sessions == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "session store must not be nil")
	case opts.Personas == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "persona directory must not be nil")
	case opts.Budgets == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "budget gate must not be nil")
	case opts.Evals == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "eval gate must not be nil")
	case opts.Handoffs == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "handoff gate must not be nil")
	case opts.AnswerTexts == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "answer extraction must not be nil")
	case opts.MaxHistoryMessages < 0 || opts.MaxHistoryBytes < 0:
		return nil, Errorf(ErrorCodeInvalidArgument, "history bounds must not be negative")
	case opts.KeepaliveInterval <= 0:
		return nil, Errorf(ErrorCodeInvalidArgument, "keepalive interval must be positive")
	case opts.Logger == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "logger must not be nil")
	}
	return &Chat{
		runtime:     opts.Runtime,
		sessions:    opts.Sessions,
		personas:    opts.Personas,
		budgets:     opts.Budgets,
		evals:       opts.Evals,
		handoffs:    opts.Handoffs,
		approvals:   opts.Approvals,
		denied:      opts.Denied,
		answerTexts: opts.AnswerTexts,
		maxMessages: opts.MaxHistoryMessages,
		maxBytes:    opts.MaxHistoryBytes,
		keepalive:   opts.KeepaliveInterval,
		logger:      opts.Logger,
	}, nil
}

func (c *Chat) ServeChat(w http.ResponseWriter, r *http.Request, call *Call) {
	ctx := r.Context()
	caller := &call.Principal
	request, spec := parseRequest(call.Body)
	if spec != nil {
		c.reject(ctx, w, spec, "invalid_request")
		return
	}
	if request.model != nil && *request.model != caller.PersonaID {
		c.reject(ctx, w, &errPersonaNotPermitted, "model_mismatch")
		return
	}
	if spec := c.admitPersona(ctx, caller); spec != nil {
		c.reject(ctx, w, spec, "persona_gate")
		return
	}
	sessionID, continuing, spec := c.resolveSession(ctx, r, caller)
	if spec != nil {
		c.reject(ctx, w, spec, "session")
		return
	}
	if continuing {
		w.Header().Set(SessionHeader, sessionID)
	}
	if spec := c.admitBudget(ctx, w, caller); spec != nil {
		c.reject(ctx, w, spec, "budget")
		return
	}
	if continuing {
		if spec := c.checkHandoff(ctx, caller, sessionID); spec != nil {
			c.reject(ctx, w, spec, "handoff")
			return
		}
	} else if err := c.sessions.Create(ctx, &Session{ID: sessionID, OwnerID: caller.ID, PersonaID: caller.PersonaID}); err != nil {
		c.logger.ErrorContext(ctx, "http session create failed", "component", "http", "error", err)
		c.reject(ctx, w, &errInternal, "session_create")
		return
	}
	w.Header().Set(SessionHeader, sessionID)
	turnRequest := &turn.TurnRequest{
		SessionID:      sessionID,
		PrincipalID:    caller.ID,
		PrincipalClass: caller.Class,
		Origin:         turn.OriginHTTP,
		Parts:          []runtimeingress.InputPart{{Text: request.final().text}},
		AgentID:        caller.PersonaID,
	}
	release := c.approvals.Hold(sessionID, &approval.Requester{Principal: caller.ID, Class: caller.Class, Persona: caller.PersonaID})
	defer release()
	if !continuing {
		turnRequest.UntrustedContext = boundedHistory(request.preceding(), c.maxMessages, c.maxBytes)
	}
	if request.stream {
		turnRequest.StreamPartial = true
		c.stream(ctx, w, caller, turnRequest, request.includeUsage)
		return
	}
	c.complete(ctx, w, caller, turnRequest)
}

func (c *Chat) admitPersona(ctx context.Context, caller *Principal) *errorSpec {
	definition, known := c.personas.Lookup(caller.PersonaID)
	if !known || !definition.Admits(caller.Class) {
		c.observeDenied(ctx, access.DeniedReasonAudience)
		return &errAudienceDenied
	}
	if caller.Class == principal.ClassOwner || definition.Audience == config.AudienceOwner {
		return nil
	}
	evaluated, err := c.evals.Evaluated(ctx, caller.PersonaID)
	if err != nil {
		c.logger.ErrorContext(ctx, "http persona eval lookup failed", "component", "http", "error", err)
		return &errInternal
	}
	if !evaluated {
		c.observeDenied(ctx, access.DeniedReasonEvalMissing)
		return &errEvalGateClosed
	}
	return nil
}

func (c *Chat) observeDenied(ctx context.Context, reason string) {
	if c.denied != nil {
		c.denied(ctx, reason)
	}
}

func (c *Chat) resolveSession(ctx context.Context, r *http.Request, caller *Principal) (id string, continuing bool, spec *errorSpec) {
	values := r.Header.Values(SessionHeader)
	if len(values) > 1 {
		return "", false, &errSessionHeader
	}
	if len(values) == 0 || values[0] == "" {
		created, err := newSessionID(caller)
		if err != nil {
			c.logger.ErrorContext(ctx, "http session id generation failed", "component", "http", "error", err)
			return "", false, &errInternal
		}
		return created, false, nil
	}
	found, spec := ownedSession(ctx, c.sessions, c.logger, caller, values[0])
	if spec != nil {
		return "", false, spec
	}
	return found, true, nil
}

func ownedSession(ctx context.Context, sessions SessionStore, logger *slog.Logger, caller *Principal, raw string) (string, *errorSpec) {
	if !ownsSessionShape(caller, raw) {
		return "", &errSessionNotFound
	}
	found, ok, err := sessions.Get(ctx, raw)
	switch {
	case err != nil:
		logger.ErrorContext(ctx, "http session lookup failed", "component", "http", "error", err)
		return "", &errInternal
	case !ok || found.OwnerID != caller.ID || found.PersonaID != caller.PersonaID:
		return "", &errSessionNotFound
	}
	return found.ID, nil
}

func sessionPrefix(persona string) string {
	return runtimeingress.SessionKey(persona, sessionSource, "")
}

func ownsSessionShape(caller *Principal, raw string) bool {
	rest, found := strings.CutPrefix(raw, sessionPrefix(caller.PersonaID))
	if !found {
		return false
	}
	keyID, token, found := strings.Cut(rest, "/")
	return found && principal.IsHTTPKeyID(keyID) && len(token) == sessionTokenLength && isKeyAlphabet(token)
}

func newSessionID(caller *Principal) (string, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", &Error{Code: ErrorCodeSessionGeneration, Detail: "random source failed", Cause: err}
	}
	token := strings.ToLower(keyEncoding.EncodeToString(raw))
	return runtimeingress.SessionKey(caller.PersonaID, sessionSource, caller.KeyID+"/"+token), nil
}

func (c *Chat) admitBudget(ctx context.Context, w http.ResponseWriter, caller *Principal) *errorSpec {
	if caller.Class == principal.ClassOwner {
		return nil
	}
	refusal, err := c.budgets.Admit(ctx, caller.ID, caller.Class)
	switch {
	case err != nil:
		c.logger.ErrorContext(ctx, "http budget check failed", "component", "http", "error", err)
		return &errInternal
	case refusal == "":
		return nil
	case refusal == access.BudgetRefusalRateLimited:
		w.Header().Set("Retry-After", rateRetryAfter)
		return &errRateLimited
	default:
		return &errBudgetExhausted
	}
}

func (c *Chat) checkHandoff(ctx context.Context, caller *Principal, sessionID string) *errorSpec {
	if caller.Class == principal.ClassOwner {
		return nil
	}
	state, err := c.handoffs.Check(ctx, sessionID)
	switch {
	case err != nil:
		c.logger.ErrorContext(ctx, "http handoff check failed", "component", "http", "error", err)
		return &errInternal
	case state == access.HandoffStateOpen:
		return &errHandoffOpen
	default:
		return nil
	}
}

type turnOutcome struct {
	turnID   string
	events   []store.RuntimeEvent
	terminal string
	failure  turn.ErrorCode
}

func (c *Chat) complete(ctx context.Context, w http.ResponseWriter, caller *Principal, turnRequest *turn.TurnRequest) {
	outcome := &turnOutcome{}
	for event, err := range c.runtime.Run(ctx, turnRequest) {
		if err != nil {
			code, _ := turn.CodeOf(err)
			outcome.terminal = turn.EventKindTurnFailed
			outcome.failure = code
			break
		}
		if outcome.turnID == "" {
			outcome.turnID = event.TurnID
		}
		outcome.events = append(outcome.events, event)
		switch event.Kind {
		case turn.EventKindTurnCompleted, turn.EventKindTurnCancelled:
			outcome.terminal = event.Kind
		case turn.EventKindTurnFailed:
			outcome.terminal = event.Kind
			outcome.failure = failureCode(event.Payload)
		}
	}
	switch outcome.terminal {
	case turn.EventKindTurnCompleted:
		c.writeCompletion(w, caller, outcome)
	case turn.EventKindTurnFailed:
		c.logger.WarnContext(ctx, "http chat turn failed", "component", "http", "code", string(outcome.failure))
		c.rejectFailure(ctx, w, outcome.failure)
	default:
		if ctx.Err() != nil {
			c.logger.InfoContext(ctx, "http chat client left before the turn ended", "component", "http")
			return
		}
		c.logger.WarnContext(ctx, "http chat turn ended without a result", "component", "http", "terminal", outcome.terminal)
		c.reject(ctx, w, &errInternal, "turn_incomplete")
	}
}

func failureCode(payload json.RawMessage) turn.ErrorCode {
	var decoded struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return turn.ErrorCodeRuntimeInternal
	}
	return turn.ErrorCode(decoded.Code)
}

func failureSpec(code turn.ErrorCode) (spec *errorSpec, reason string) {
	switch code {
	case turn.ErrorCodeRuntimeOverloaded:
		return &errRuntimeOverloaded, "turn_overloaded"
	case turn.ErrorCodeBudgetExhausted:
		return &errBudgetExhausted, "turn_budget"
	default:
		return &errInternal, "turn_failed"
	}
}

func (c *Chat) rejectFailure(ctx context.Context, w http.ResponseWriter, code turn.ErrorCode) {
	spec, reason := failureSpec(code)
	if code == turn.ErrorCodeRuntimeOverloaded {
		w.Header().Set("Retry-After", overloadRetryAfter)
	}
	c.reject(ctx, w, spec, reason)
}

func (c *Chat) reject(ctx context.Context, w http.ResponseWriter, spec *errorSpec, reason string) {
	c.logger.WarnContext(ctx, "http request rejected", "component", "http", "reason", reason, "status", spec.status)
	writeError(w, spec)
}

type completion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   usage    `json:"usage"`
}

type choice struct {
	Index        int              `json:"index"`
	Message      assistantMessage `json:"message"`
	FinishReason string           `json:"finish_reason"`
}

type assistantMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func (c *Chat) writeCompletion(w http.ResponseWriter, caller *Principal, outcome *turnOutcome) {
	consumed := tokenUsage(outcome.events)
	writeJSON(w, http.StatusOK, &completion{
		ID:      "chatcmpl-" + outcome.turnID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   caller.PersonaID,
		Choices: []choice{{
			Index:        0,
			Message:      assistantMessage{Role: roleAssistant, Content: strings.Join(c.answerTexts(outcome.events), answerSeparator)},
			FinishReason: finishReason(outcome.events),
		}},
		Usage: consumed,
	})
}

func tokenUsage(events []store.RuntimeEvent) usage {
	var total usage
	for i := range events {
		if len(events[i].ProviderUsage) == 0 {
			continue
		}
		var reported struct {
			Prompt     int64 `json:"promptTokenCount"`
			Candidates int64 `json:"candidatesTokenCount"`
			Thoughts   int64 `json:"thoughtsTokenCount"`
		}
		if err := json.Unmarshal(events[i].ProviderUsage, &reported); err != nil {
			continue
		}
		total.PromptTokens += reported.Prompt
		total.CompletionTokens += reported.Candidates + reported.Thoughts
	}
	total.TotalTokens = total.PromptTokens + total.CompletionTokens
	return total
}

func finishReason(events []store.RuntimeEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != store.EventKindADK {
			continue
		}
		var decoded struct {
			Content *struct {
				Role string `json:"role"`
			} `json:"content"`
			Partial      bool   `json:"partial"`
			FinishReason string `json:"finishReason"`
		}
		if err := json.Unmarshal(events[i].Payload, &decoded); err != nil || decoded.Partial {
			continue
		}
		if decoded.Content == nil || decoded.Content.Role != genai.RoleModel {
			continue
		}
		if decoded.FinishReason == string(genai.FinishReasonMaxTokens) {
			return finishLength
		}
		return finishStop
	}
	return finishStop
}

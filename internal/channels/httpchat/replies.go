package httpchat

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/anggasct/aura/internal/channels/kit/access"
)

const (
	routeHandoffReplies = "/v1/handoff/replies"

	afterParameter    = "after"
	maxHandoffReplies = 100
	objectList        = "list"
	objectReply       = "handoff.reply"
)

type HandoffReply struct {
	Sequence  uint64
	Text      string
	CreatedAt time.Time
}

type HandoffReplySource interface {
	List(ctx context.Context, sessionID string, afterSequence uint64, limit int) ([]HandoffReply, error)
}

type RepliesHandler interface {
	ServeReplies(w http.ResponseWriter, r *http.Request, caller *Principal)
}

type RepliesOptions struct {
	Sessions SessionStore
	Handoffs access.HandoffGate
	Replies  HandoffReplySource
	Logger   *slog.Logger
}

type Replies struct {
	sessions SessionStore
	handoffs access.HandoffGate
	replies  HandoffReplySource
	logger   *slog.Logger
}

var _ RepliesHandler = (*Replies)(nil)

func NewReplies(opts *RepliesOptions) (*Replies, error) {
	switch {
	case opts == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "replies options must not be nil")
	case opts.Sessions == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "session store must not be nil")
	case opts.Handoffs == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "handoff gate must not be nil")
	case opts.Replies == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "reply source must not be nil")
	case opts.Logger == nil:
		return nil, Errorf(ErrorCodeInvalidArgument, "logger must not be nil")
	}
	return &Replies{sessions: opts.Sessions, handoffs: opts.Handoffs, replies: opts.Replies, logger: opts.Logger}, nil
}

func (p *Replies) ServeReplies(w http.ResponseWriter, r *http.Request, caller *Principal) {
	ctx := r.Context()
	raw, spec := requiredSessionHeader(r)
	if spec != nil {
		p.reject(ctx, w, spec, "session_header")
		return
	}
	after, spec := parseAfter(r)
	if spec != nil {
		p.reject(ctx, w, spec, "cursor")
		return
	}
	sessionID, spec := ownedSession(ctx, p.sessions, p.logger, caller, raw)
	if spec != nil {
		p.reject(ctx, w, spec, "session")
		return
	}
	stored, err := p.replies.List(ctx, sessionID, after, maxHandoffReplies)
	if err != nil {
		p.logger.ErrorContext(ctx, "http handoff replies lookup failed", "component", "http", "error", err)
		p.reject(ctx, w, &errInternal, "replies_lookup")
		return
	}
	state, err := p.handoffs.Check(ctx, sessionID)
	if err != nil {
		p.logger.ErrorContext(ctx, "http handoff check failed", "component", "http", "error", err)
		p.reject(ctx, w, &errInternal, "handoff_check")
		return
	}
	data := make([]replyItem, 0, len(stored))
	for i := range stored {
		data = append(data, replyItem{
			ID:      strconv.FormatUint(stored[i].Sequence, 10),
			Object:  objectReply,
			Content: stored[i].Text,
			Created: stored[i].CreatedAt.Unix(),
		})
	}
	writeJSON(w, http.StatusOK, &replyList{Object: objectList, Data: data, HandoffOpen: state == access.HandoffStateOpen})
}

func (p *Replies) reject(ctx context.Context, w http.ResponseWriter, spec *errorSpec, reason string) {
	p.logger.WarnContext(ctx, "http request rejected", "component", "http", "reason", reason, "status", spec.status)
	writeError(w, spec)
}

func requiredSessionHeader(r *http.Request) (string, *errorSpec) {
	values := r.Header.Values(SessionHeader)
	switch {
	case len(values) > 1:
		return "", &errSessionHeader
	case len(values) == 0 || values[0] == "":
		return "", &errSessionRequired
	}
	return values[0], nil
}

func parseAfter(r *http.Request) (uint64, *errorSpec) {
	values, present := r.URL.Query()[afterParameter]
	if !present {
		return 0, nil
	}
	if len(values) != 1 {
		return 0, &errCursorInvalid
	}
	cursor, err := strconv.ParseUint(values[0], 10, 64)
	if err != nil || cursor > math.MaxInt64 {
		return 0, &errCursorInvalid
	}
	return cursor, nil
}

type replyList struct {
	Object      string      `json:"object"`
	Data        []replyItem `json:"data"`
	HandoffOpen bool        `json:"handoff_open"`
}

type replyItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Content string `json:"content"`
	Created int64  `json:"created"`
}

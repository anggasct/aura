package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	sessionTransportQueueDepth = 64
	sessionPollPause           = 25 * time.Millisecond
	sessionPollPrefix          = "aura-sb-poll-"
)

type ContainedSession interface {
	Request(ctx context.Context, payload []byte) ([]byte, error)
	Close(ctx context.Context) error
}

type ContainedSessionRequest struct {
	RequestID      string
	ServerName     string
	Executable     string
	Arguments      []string
	WorkingDir     string
	Environment    map[string]string
	Timeout        time.Duration
	MaxOutputBytes int64
}

type SessionStarter interface {
	StartSession(ctx context.Context, req *ContainedSessionRequest) (ContainedSession, error)
}

type sessionTransport struct {
	session ContainedSession
	server  string

	queueMu sync.Mutex
	queue   [][]byte
	readMu  sync.Mutex
	polls   atomic.Uint64
	closed  atomic.Bool
}

func (c *Client) containedTransport(ctx context.Context) (sdk.Transport, error) {
	if ctx == nil {
		return nil, Errorf(ErrConfigInvalid, "context must not be nil")
	}
	if c.sessionStarter == nil {
		return nil, Errorf(ErrServerUnavailable, "server %q has no session containment configured", c.cfg.Name)
	}
	workingDir, err := os.Getwd()
	if err != nil {
		return nil, Errorf(ErrServerUnavailable, "working directory is not available")
	}
	environment := make(map[string]string, len(c.cfg.Environment))
	for key, value := range c.cfg.Environment {
		environment[key] = value
	}
	session, err := c.sessionStarter.StartSession(ctx, &ContainedSessionRequest{
		RequestID:      containedRequestID(c.cfg.Name),
		ServerName:     c.cfg.Name,
		Executable:     c.cfg.Command,
		Arguments:      append([]string(nil), c.cfg.Args...),
		WorkingDir:     workingDir,
		Environment:    environment,
		Timeout:        time.Duration(c.cfg.RequestTimeout),
		MaxOutputBytes: maxMessageBytes(c.cfg),
	})
	if err != nil {
		return nil, Errorf(ErrServerUnavailable, "server %q containment failed", c.cfg.Name)
	}
	transport, err := newSessionTransport(session, c.cfg.Name)
	if err != nil {
		_ = session.Close(ctx)
		return nil, err
	}
	c.containedSession = session
	return transport, nil
}

var containedSessionSeq atomic.Uint64

func containedRequestID(server string) string {
	return fmt.Sprintf("mcp-stdio-%s-%d", server, containedSessionSeq.Add(1))
}

func newSessionTransport(session ContainedSession, server string) (*sessionTransport, error) {
	if session == nil {
		return nil, Errorf(ErrConfigInvalid, "contained session must not be nil")
	}
	if strings.TrimSpace(server) == "" {
		return nil, Errorf(ErrConfigInvalid, "server name must not be empty")
	}
	return &sessionTransport{session: session, server: server}, nil
}

func (t *sessionTransport) Connect(_ context.Context) (sdk.Connection, error) {
	if t.closed.Load() {
		return nil, Errorf(ErrServerUnavailable, "session transport is closed")
	}
	return &sessionConn{transport: t}, nil
}

type sessionConn struct {
	transport *sessionTransport
}

func (c *sessionConn) SessionID() string {
	return ""
}

func (c *sessionConn) Close() error {
	c.transport.closed.Store(true)
	return nil
}

func (c *sessionConn) Write(_ context.Context, msg jsonrpc.Message) error {
	if msg == nil {
		return Errorf(ErrConfigInvalid, "message must not be nil")
	}
	raw, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return Wrap(ErrResultInvalid, err, "message is not encodable")
	}
	transport := c.transport
	if transport.closed.Load() {
		return Errorf(ErrServerUnavailable, "session transport for server %q is closed", transport.server)
	}
	transport.queueMu.Lock()
	defer transport.queueMu.Unlock()
	if len(transport.queue) >= sessionTransportQueueDepth {
		return Errorf(ErrServerUnavailable, "session transport queue for server %q is full", transport.server)
	}
	transport.queue = append(transport.queue, raw)
	return nil
}

type rpcHeader struct {
	ID     any    `json:"id"`
	Method string `json:"method"`
}

func (c *sessionConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	if ctx == nil {
		return nil, Errorf(ErrConfigInvalid, "context must not be nil")
	}
	transport := c.transport
	transport.readMu.Lock()
	defer transport.readMu.Unlock()
	for {
		if transport.closed.Load() {
			return nil, Errorf(ErrServerUnavailable, "session transport for server %q is closed", transport.server)
		}
		frame, polled := transport.nextTurn()
		line, err := transport.session.Request(ctx, frame)
		if err != nil {
			return nil, err
		}
		msg, err := jsonrpc.DecodeMessage(line)
		if err != nil {
			return nil, Errorf(ErrResultInvalid, "contained server sent a malformed message")
		}
		if isPollAck(msg) {
			if !polled {
				continue
			}
			if err := pauseForPoll(ctx); err != nil {
				return nil, err
			}
			continue
		}
		return msg, nil
	}
}

func (t *sessionTransport) nextTurn() ([]byte, bool) {
	t.queueMu.Lock()
	defer t.queueMu.Unlock()
	frames := make([][]byte, 0, len(t.queue)+1)
	frames = append(frames, t.queue...)
	for i := range t.queue {
		t.queue[i] = nil
	}
	t.queue = t.queue[:0]
	poll := fmt.Sprintf("%s%d", sessionPollPrefix, t.polls.Add(1))
	frames = append(frames, []byte(`{"jsonrpc":"2.0","id":"`+poll+`","method":"ping"}`))
	return bytes.Join(frames, []byte("\n")), len(frames) == 1
}

func isPollAck(msg jsonrpc.Message) bool {
	if msg == nil {
		return false
	}
	raw, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return false
	}
	var header rpcHeader
	if err := json.Unmarshal(raw, &header); err != nil {
		return false
	}
	if header.Method != "" {
		return false
	}
	id, ok := header.ID.(string)
	return ok && strings.HasPrefix(id, sessionPollPrefix)
}

func pauseForPoll(ctx context.Context) error {
	timer := time.NewTimer(sessionPollPause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

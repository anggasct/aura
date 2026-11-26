package mcp

import (
	"context"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
)

const (
	ResultConnected    = "connected"
	ResultDisconnected = "disconnected"
	ResultReconnected  = "reconnected"
	ResultToolCall     = "tool_call"
	ResultDiscovered   = "discovered"
	ResultFailed       = "failed"
)

const ResultCodeOK = "ok"

type Observation struct {
	Server          string
	Transport       string
	ProtocolVersion string
	Tool            string
	Count           int
	Result          string
	ResultCode      string
	SizeBytes       int64
	Duration        time.Duration
}

type Observer func(ctx context.Context, observation *Observation)

func (c *Client) observe(ctx context.Context, observation *Observation) {
	if c.observer == nil || observation == nil {
		return
	}
	c.observer(ctx, observation)
}

func (c *Client) observeFailure(ctx context.Context, start time.Time, version, tool string, err error) {
	code := string(ErrServerUnavailable)
	if failureCode, ok := fault.CodeOf[ErrorCode](err); ok {
		code = string(failureCode)
	}
	c.observe(ctx, &Observation{
		Server:          c.cfg.Name,
		Transport:       c.cfg.Transport,
		ProtocolVersion: version,
		Tool:            tool,
		Result:          ResultFailed,
		ResultCode:      code,
		Duration:        time.Since(start),
	})
}

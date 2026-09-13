package mcp

import (
	"context"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func (c *Client) reconnectBudget() (maxAttempts int, window time.Duration, ok bool) {
	restart := c.cfg.Restart
	if restart == nil || restart.MaxAttempts <= 0 {
		return 0, 0, false
	}
	return restart.MaxAttempts, time.Duration(restart.Window), true
}

func (c *Client) reconnectAllowedLocked(now time.Time) bool {
	maxAttempts, window, ok := c.reconnectBudget()
	if !ok {
		return false
	}
	if window > 0 {
		kept := c.restarts[:0]
		for _, attempt := range c.restarts {
			if now.Sub(attempt) < window {
				kept = append(kept, attempt)
			}
		}
		c.restarts = kept
	}
	if len(c.restarts) >= maxAttempts {
		return false
	}
	c.restarts = append(c.restarts, now)
	return true
}

func (c *Client) resetReconnectsLocked() {
	c.restarts = nil
}

func (c *Client) ensureConnected(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Errorf(ErrServerUnavailable, "client is already closed")
	}
	if c.session != nil {
		return nil
	}
	if c.customTransport {
		return Errorf(ErrServerUnavailable, "client session is not connected")
	}
	if !c.reconnectAllowedLocked(time.Now()) {
		return Errorf(ErrServerUnavailable, "server %q reconnect budget exhausted", c.cfg.Name)
	}
	return c.connectLocked(ctx, nil)
}

func (c *Client) noteDeadLocked(ctx context.Context) {
	if c.session != nil {
		_ = c.session.Close()
		c.session = nil
	}
	c.closeContainedLocked(ctx)
}

func (c *Client) noteDead(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.noteDeadLocked(ctx)
}

func (c *Client) dialSession(ctx context.Context, sdkClient *sdk.Client, transport sdk.Transport, timeout time.Duration) (*sdk.ClientSession, context.CancelFunc, error) {
	if _, isSSE := transport.(*sdk.SSEClientTransport); !isSSE {
		connectCtx := ctx
		var cancel context.CancelFunc
		if timeout > 0 {
			connectCtx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		session, err := sdkClient.Connect(connectCtx, transport, nil)
		if err != nil {
			return nil, nil, Wrap(ErrServerUnavailable, err, "failed to connect to server")
		}
		return session, nil, nil
	}
	streamCtx, streamCancel := context.WithCancel(context.WithoutCancel(ctx))
	type dialResult struct {
		session *sdk.ClientSession
		err     error
	}
	delivered := make(chan dialResult, 1)
	go func() {
		session, err := sdkClient.Connect(streamCtx, transport, nil)
		delivered <- dialResult{session: session, err: err}
	}()
	var timer <-chan time.Time
	if timeout > 0 {
		ticker := time.NewTimer(timeout)
		defer ticker.Stop()
		timer = ticker.C
	}
	select {
	case res := <-delivered:
		if res.err != nil {
			streamCancel()
			return nil, nil, Wrap(ErrServerUnavailable, res.err, "failed to connect to server")
		}
		return res.session, streamCancel, nil
	case <-ctx.Done():
		streamCancel()
		go func() {
			if res := <-delivered; res.session != nil {
				_ = res.session.Close()
			}
		}()
		return nil, nil, Wrap(ErrServerUnavailable, ctx.Err(), "legacy connect cancelled")
	case <-timer:
		streamCancel()
		go func() {
			if res := <-delivered; res.session != nil {
				_ = res.session.Close()
			}
		}()
		return nil, nil, Errorf(ErrRequestTimeout, "server %q connect timed out", c.cfg.Name)
	}
}

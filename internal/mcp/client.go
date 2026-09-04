package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/anggasct/aura/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type Client struct {
	cfg       *config.MCPServer
	logger    *slog.Logger
	session   *sdk.ClientSession
	transport sdk.Transport
	mu        sync.Mutex
	closed    bool
}

func NewClient(cfg *config.MCPServer, logger *slog.Logger) (*Client, error) {
	if cfg == nil {
		return nil, Errorf(ErrConfigInvalid, "server configuration is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		cfg:    cfg,
		logger: logger,
	}, nil
}

func (c *Client) Connect(ctx context.Context, customTransport sdk.Transport) error {
	if ctx == nil {
		return Errorf(ErrConfigInvalid, "context must not be nil")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Errorf(ErrServerUnavailable, "client is already closed")
	}

	transport := customTransport
	if transport == nil {
		switch c.cfg.Transport {
		case config.MCPTransportStdio:
			cmd := exec.CommandContext(ctx, c.cfg.Command, c.cfg.Args...)
			cmd.Env = buildEnvironment(c.cfg.Environment)
			transport = &sdk.CommandTransport{
				Command: cmd,
			}
		default:
			return Errorf(ErrConfigInvalid, "unsupported transport: %s", c.cfg.Transport)
		}
	}
	c.transport = transport

	sdkClient := sdk.NewClient(&sdk.Implementation{
		Name:    "aura",
		Version: "v1.0.0",
	}, &sdk.ClientOptions{
		Logger: c.logger,
	})

	connectCtx := ctx
	var cancel context.CancelFunc
	if c.cfg.StartupTimeout > 0 {
		connectCtx, cancel = context.WithTimeout(ctx, time.Duration(c.cfg.StartupTimeout))
		defer cancel()
	}

	c.logger.InfoContext(ctx, "connecting to server",
		"component", "mcp_client",
		"server", c.cfg.Name,
		"transport", c.cfg.Transport,
	)

	session, err := sdkClient.Connect(connectCtx, transport, nil)
	if err != nil {
		if strings.Contains(err.Error(), "unsupported protocol version") {
			return Errorf(ErrProtocolUnsupported, "%v", err)
		}
		return Wrap(ErrServerUnavailable, err, "failed to connect to server")
	}
	c.session = session

	initResult := session.InitializeResult()
	if initResult == nil {
		_ = session.Close()
		return Errorf(ErrServerUnavailable, "no initialize result from server")
	}

	if !IsSupportedProtocolVersion(initResult.ProtocolVersion) {
		_ = session.Close()
		return Errorf(ErrProtocolUnsupported, "unsupported protocol version: %s", initResult.ProtocolVersion)
	}

	if initResult.Capabilities == nil || initResult.Capabilities.Tools == nil {
		_ = session.Close()
		return Errorf(ErrCapabilityUnavailable, "server does not advertise tool capabilities")
	}

	c.logger.InfoContext(ctx, "server connected",
		"component", "mcp_client",
		"server", c.cfg.Name,
		"protocol_version", initResult.ProtocolVersion,
	)

	return nil
}

func (c *Client) DiscoverTools(ctx context.Context) ([]DiscoveredTool, error) {
	if ctx == nil {
		return nil, Errorf(ErrConfigInvalid, "context must not be nil")
	}

	c.mu.Lock()
	session := c.session
	c.mu.Unlock()

	if session == nil {
		return nil, Errorf(ErrServerUnavailable, "client session not connected")
	}

	listCtx := ctx
	var cancel context.CancelFunc
	if c.cfg.RequestTimeout > 0 {
		listCtx, cancel = context.WithTimeout(ctx, time.Duration(c.cfg.RequestTimeout))
		defer cancel()
	}

	toolsResult, err := session.ListTools(listCtx, nil)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, Errorf(ErrRequestTimeout, "list tools request timed out: %v", err)
		}
		return nil, Wrap(ErrServerUnavailable, err, "failed to list tools")
	}

	seen := make(map[string]bool)
	discovered := make([]DiscoveredTool, 0, len(toolsResult.Tools))
	maxMsgSize := int64(c.cfg.MaxMessageSize)
	if maxMsgSize <= 0 {
		maxMsgSize = 1 << 20
	}

	for _, t := range toolsResult.Tools {
		if t == nil {
			continue
		}
		if strings.TrimSpace(t.Name) == "" {
			return nil, Errorf(ErrSchemaInvalid, "tool has empty name")
		}
		if seen[t.Name] {
			return nil, Errorf(ErrSchemaInvalid, "duplicate tool name %q", t.Name)
		}
		seen[t.Name] = true

		schemaBytes, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, Wrap(ErrSchemaInvalid, err, fmt.Sprintf("invalid schema for tool %q", t.Name))
		}
		if int64(len(schemaBytes)) > maxMsgSize {
			return nil, Errorf(ErrMessageTooLarge, "tool %q schema exceeds max message size", t.Name)
		}

		discovered = append(discovered, DiscoveredTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schemaBytes,
		})
	}

	return discovered, nil
}

func (c *Client) CallTool(ctx context.Context, toolName string, arguments json.RawMessage) (*sdk.CallToolResult, error) {
	if ctx == nil {
		return nil, Errorf(ErrConfigInvalid, "context must not be nil")
	}

	c.mu.Lock()
	session := c.session
	c.mu.Unlock()

	if session == nil {
		return nil, Errorf(ErrServerUnavailable, "client session not connected")
	}

	var args any
	if len(arguments) > 0 {
		var m any
		if err := json.Unmarshal(arguments, &m); err != nil {
			return nil, Wrap(ErrResultInvalid, err, "invalid tool arguments JSON")
		}
		args = m
	}

	params := &sdk.CallToolParams{
		Name:      toolName,
		Arguments: args,
	}

	callCtx := ctx
	var cancel context.CancelFunc
	if c.cfg.RequestTimeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, time.Duration(c.cfg.RequestTimeout))
		defer cancel()
	}

	res, err := session.CallTool(callCtx, params)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, Errorf(ErrRequestTimeout, "tool call timed out: %v", err)
		}
		return nil, Wrap(ErrServerUnavailable, err, "tool call failed")
	}
	return res, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.session != nil {
		return c.session.Close()
	}
	return nil
}

func (c *Client) ServerName() string {
	if c.cfg == nil {
		return ""
	}
	return c.cfg.Name
}

func buildEnvironment(env map[string]string) []string {
	result := make([]string, 0, len(env))
	for k, v := range env {
		result = append(result, k+"="+v)
	}
	slices.Sort(result)
	return result
}

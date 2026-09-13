package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/anggasct/aura/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type Client struct {
	cfg              *config.MCPServer
	logger           *slog.Logger
	session          *sdk.ClientSession
	transport        sdk.Transport
	httpClient       *http.Client
	apiClient        *http.Client
	endpointPolicy   EndpointPolicy
	secretResolver   SecretResolver
	oauthFetch       sdkauth.AuthorizationCodeFetcher
	observer         Observer
	sessionStarter   SessionStarter
	containedSession ContainedSession
	streamCancel     context.CancelFunc
	protocolVersion  string
	restarts         []time.Time
	customTransport  bool
	mu               sync.Mutex
	closed           bool
}

func NewClient(cfg *config.MCPServer, logger *slog.Logger, opts ...ClientOption) (*Client, error) {
	if cfg == nil {
		return nil, Errorf(ErrConfigInvalid, "server configuration is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	client := &Client{
		cfg:    cfg,
		logger: logger,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(client)
		}
	}
	return client, nil
}

func (c *Client) Connect(ctx context.Context, customTransport sdk.Transport) error {
	if ctx == nil {
		return Errorf(ErrConfigInvalid, "context must not be nil")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectLocked(ctx, customTransport)
}

func (c *Client) connectLocked(ctx context.Context, customTransport sdk.Transport) error {
	start := time.Now()
	if c.closed {
		return Errorf(ErrServerUnavailable, "client is already closed")
	}

	transport := customTransport
	if transport == nil {
		switch c.cfg.Transport {
		case config.MCPTransportStdio:
			contained, err := c.containedTransport(ctx)
			if err != nil {
				c.observeFailure(ctx, start, c.protocolVersion, "", err)
				return err
			}
			transport = contained
		case config.MCPTransportStreamableHTTP:
			streamable, err := c.streamableTransport(ctx)
			if err != nil {
				c.observeFailure(ctx, start, c.protocolVersion, "", err)
				return err
			}
			transport = streamable
		case config.MCPTransportLegacySSE:
			legacy, err := c.legacySSETransport(ctx)
			if err != nil {
				c.observeFailure(ctx, start, c.protocolVersion, "", err)
				return err
			}
			transport = legacy
		default:
			return Errorf(ErrConfigInvalid, "unsupported transport: %s", c.cfg.Transport)
		}
	}
	c.transport = transport
	c.customTransport = customTransport != nil

	sdkClient := sdk.NewClient(&sdk.Implementation{
		Name:    "aura",
		Version: "v1.0.0",
	}, &sdk.ClientOptions{
		Logger: c.logger,
	})

	connectTimeout := c.cfg.StartupTimeout
	if c.cfg.Transport == config.MCPTransportStreamableHTTP && c.cfg.ConnectTimeout > 0 {
		connectTimeout = c.cfg.ConnectTimeout
	}

	c.logger.InfoContext(ctx, "connecting to server",
		"component", "mcp_client",
		"server", c.cfg.Name,
		"transport", c.cfg.Transport,
	)

	session, streamCancel, err := c.dialSession(ctx, sdkClient, transport, time.Duration(connectTimeout))
	if err != nil {
		c.closeContainedLocked(ctx)
		c.observeFailure(ctx, start, c.protocolVersion, "", err)
		return err
	}
	if streamCancel != nil {
		if c.streamCancel != nil {
			c.streamCancel()
		}
		c.streamCancel = streamCancel
	}
	c.session = session

	initResult := session.InitializeResult()
	if initResult == nil {
		_ = session.Close()
		c.session = nil
		c.closeContainedLocked(ctx)
		err := Errorf(ErrServerUnavailable, "no initialize result from server")
		c.observeFailure(ctx, start, c.protocolVersion, "", err)
		return err
	}

	if !IsSupportedProtocolVersion(initResult.ProtocolVersion) {
		_ = session.Close()
		c.session = nil
		c.closeContainedLocked(ctx)
		err := Errorf(ErrProtocolUnsupported, "unsupported protocol version: %s", initResult.ProtocolVersion)
		c.observeFailure(ctx, start, c.protocolVersion, "", err)
		return err
	}

	if initResult.Capabilities == nil || initResult.Capabilities.Tools == nil {
		_ = session.Close()
		c.session = nil
		c.closeContainedLocked(ctx)
		err := Errorf(ErrCapabilityUnavailable, "server does not advertise tool capabilities")
		c.observeFailure(ctx, start, c.protocolVersion, "", err)
		return err
	}

	c.protocolVersion = initResult.ProtocolVersion
	c.resetReconnectsLocked()

	c.logger.InfoContext(ctx, "server connected",
		"component", "mcp_client",
		"server", c.cfg.Name,
		"protocol_version", initResult.ProtocolVersion,
	)
	c.observe(ctx, &Observation{
		Server:          c.cfg.Name,
		Transport:       c.cfg.Transport,
		ProtocolVersion: initResult.ProtocolVersion,
		Result:          ResultConnected,
		ResultCode:      ResultCodeOK,
		Duration:        time.Since(start),
	})

	return nil
}

func maxMessageBytes(cfg *config.MCPServer) int64 {
	if cfg != nil && cfg.MaxMessageSize > 0 {
		return int64(cfg.MaxMessageSize)
	}
	return 1 << 20
}

func (c *Client) streamRetries() int {
	if maxAttempts, _, ok := c.reconnectBudget(); ok {
		return maxAttempts
	}
	return -1
}

func (c *Client) checkedHTTPClient(ctx context.Context) (*http.Client, error) {
	if strings.TrimSpace(c.cfg.URL) == "" {
		return nil, Errorf(ErrConfigInvalid, "url is required for http transport")
	}
	if c.endpointPolicy == nil {
		return nil, Errorf(ErrEgressDenied, "server %q has no endpoint policy configured", c.cfg.Name)
	}
	if err := c.endpointPolicy.ValidateEndpoint(ctx, c.cfg.URL); err != nil {
		return nil, Wrap(ErrEgressDenied, err, "server endpoint rejected")
	}
	if c.httpClient == nil {
		return nil, Errorf(ErrEgressDenied, "server %q has no http client configured", c.cfg.Name)
	}
	return c.httpClient, nil
}

func (c *Client) legacySSETransport(ctx context.Context) (sdk.Transport, error) {
	base, err := c.checkedHTTPClient(ctx)
	if err != nil {
		return nil, err
	}
	if c.cfg.Auth != nil && c.cfg.Auth.OAuth != nil {
		return nil, Errorf(ErrCapabilityUnavailable, "oauth is not supported on the legacy sse transport")
	}
	allowRedirects := c.cfg.AllowRedirects != nil && *c.cfg.AllowRedirects
	baseCheck := base.CheckRedirect
	roundTripper := base.Transport
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	if c.cfg.Auth != nil && c.cfg.Auth.Static != nil {
		if c.secretResolver == nil {
			return nil, Errorf(ErrAuthRequired, "server %q has no credential resolver", c.cfg.Name)
		}
		if _, err := c.secretResolver.ResolveSecret(ctx, c.cfg.Auth.Static.CredentialRef); err != nil {
			return nil, Errorf(ErrAuthRequired, "server %q credential is unavailable", c.cfg.Name)
		}
		roundTripper = &staticAuthTransport{
			next:       roundTripper,
			header:     c.cfg.Auth.Static.Header,
			ref:        c.cfg.Auth.Static.CredentialRef,
			serverName: c.cfg.Name,
			resolver:   c.secretResolver,
		}
	}
	apiClient := &http.Client{
		Transport: &cappedTransport{next: roundTripper, limit: maxMessageBytes(c.cfg)},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return checkRedirectPolicy(req, via, allowRedirects, baseCheck)
		},
	}
	c.apiClient = apiClient
	return &sdk.SSEClientTransport{Endpoint: c.cfg.URL, HTTPClient: apiClient}, nil
}

func (c *Client) streamableTransport(ctx context.Context) (sdk.Transport, error) {
	base, err := c.checkedHTTPClient(ctx)
	if err != nil {
		return nil, err
	}
	allowRedirects := c.cfg.AllowRedirects != nil && *c.cfg.AllowRedirects
	baseCheck := base.CheckRedirect
	roundTripper := base.Transport
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	capped := &cappedTransport{next: roundTripper, limit: maxMessageBytes(c.cfg)}
	apiClient := &http.Client{
		Transport: capped,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return checkRedirectPolicy(req, via, allowRedirects, baseCheck)
		},
	}
	var handler *sdkauth.AuthorizationCodeHandler
	if c.cfg.Auth != nil {
		if c.cfg.Auth.Static != nil && c.cfg.Auth.OAuth != nil {
			return nil, Errorf(ErrConfigInvalid, "server %q declares both static and oauth auth", c.cfg.Name)
		}
		if c.cfg.Auth.Static != nil {
			if c.secretResolver == nil {
				return nil, Errorf(ErrAuthRequired, "server %q has no credential resolver", c.cfg.Name)
			}
			if _, err := c.secretResolver.ResolveSecret(ctx, c.cfg.Auth.Static.CredentialRef); err != nil {
				return nil, Errorf(ErrAuthRequired, "server %q credential is unavailable", c.cfg.Name)
			}
			capped.next = &staticAuthTransport{
				next:       roundTripper,
				header:     c.cfg.Auth.Static.Header,
				ref:        c.cfg.Auth.Static.CredentialRef,
				serverName: c.cfg.Name,
				resolver:   c.secretResolver,
			}
		}
		if c.cfg.Auth.OAuth != nil {
			oauthClient := &http.Client{
				Transport: capped,
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					return checkRedirectPolicy(req, via, false, baseCheck)
				},
			}
			provider, err := c.oauthProvider(oauthClient)
			if err != nil {
				return nil, err
			}
			handler, err = provider.Handler(ctx)
			if err != nil {
				return nil, err
			}
		}
	}
	transport := &sdk.StreamableClientTransport{
		Endpoint:   c.cfg.URL,
		HTTPClient: apiClient,
		MaxRetries: c.streamRetries(),
	}
	c.apiClient = apiClient
	if handler != nil {
		transport.OAuthHandler = handler
	}
	return transport, nil
}

func (c *Client) oauthProvider(oauthClient *http.Client) (*OAuthProvider, error) {
	oauth := c.cfg.Auth.OAuth
	clientID := strings.TrimSpace(os.Getenv(oauth.ClientIDEnv))
	clientSecret := strings.TrimSpace(os.Getenv(oauth.ClientSecretEnv))
	if clientID == "" || clientSecret == "" {
		return nil, Errorf(ErrAuthRequired, "server %q oauth client credentials are not configured", c.cfg.Name)
	}
	var tokens OAuthTokenStore = NewMemoryTokenStore()
	if strings.TrimSpace(oauth.TokenStore) != "" {
		fileStore, err := NewFileTokenStore(oauth.TokenStore)
		if err != nil {
			return nil, err
		}
		tokens = fileStore
	}
	return NewOAuthProvider(c.cfg.Name, c.cfg.URL, clientID, clientSecret, oauthClient, tokens, c.oauthFetch)
}

func (c *Client) DiscoverTools(ctx context.Context) ([]DiscoveredTool, error) {
	if ctx == nil {
		return nil, Errorf(ErrConfigInvalid, "context must not be nil")
	}
	start := time.Now()

	c.mu.Lock()
	version := c.protocolVersion
	c.mu.Unlock()

	if err := c.ensureConnected(ctx); err != nil {
		c.observeFailure(ctx, start, version, "", err)
		return nil, err
	}

	c.mu.Lock()
	session := c.session
	version = c.protocolVersion
	c.mu.Unlock()

	if session == nil {
		err := Errorf(ErrServerUnavailable, "client session is not connected")
		c.observeFailure(ctx, start, version, "", err)
		return nil, err
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
			c.noteDead(ctx)
			err := Errorf(ErrRequestTimeout, "list tools request timed out")
			c.observeFailure(ctx, start, version, "", err)
			return nil, err
		}
		if isResponseTooLarge(err) {
			err := Errorf(ErrMessageTooLarge, "tool listing exceeds the configured message bound")
			c.observeFailure(ctx, start, version, "", err)
			return nil, err
		}
		c.noteDead(ctx)
		err := Wrap(ErrServerUnavailable, err, "failed to list tools")
		c.observeFailure(ctx, start, version, "", err)
		return nil, err
	}

	seen := make(map[string]bool)
	discovered := make([]DiscoveredTool, 0, len(toolsResult.Tools))
	maxMsgSize := maxMessageBytes(c.cfg)
	var discoverySize int64

	for _, t := range toolsResult.Tools {
		if t == nil {
			continue
		}
		if strings.TrimSpace(t.Name) == "" {
			return nil, Errorf(ErrSchemaInvalid, "tool has empty name")
		}
		if err := validateDiscoveredToolName(t.Name); err != nil {
			return nil, err
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
		if err := validateToolSchema(t.Name, schemaBytes); err != nil {
			return nil, err
		}
		discoverySize += int64(len(schemaBytes))

		discovered = append(discovered, DiscoveredTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schemaBytes,
		})
	}

	c.mu.Lock()
	c.resetReconnectsLocked()
	c.mu.Unlock()
	c.observe(ctx, &Observation{
		Server:          c.cfg.Name,
		Transport:       c.cfg.Transport,
		ProtocolVersion: version,
		Count:           len(discovered),
		Result:          ResultDiscovered,
		ResultCode:      ResultCodeOK,
		SizeBytes:       discoverySize,
		Duration:        time.Since(start),
	})

	return discovered, nil
}

func (c *Client) CallTool(ctx context.Context, toolName string, arguments json.RawMessage) (*sdk.CallToolResult, error) {
	if ctx == nil {
		return nil, Errorf(ErrConfigInvalid, "context must not be nil")
	}
	start := time.Now()

	c.mu.Lock()
	version := c.protocolVersion
	c.mu.Unlock()

	if err := c.ensureConnected(ctx); err != nil {
		c.observeFailure(ctx, start, version, toolName, err)
		return nil, err
	}

	c.mu.Lock()
	session := c.session
	version = c.protocolVersion
	c.mu.Unlock()

	if session == nil {
		err := Errorf(ErrServerUnavailable, "client session not connected")
		c.observeFailure(ctx, start, version, toolName, err)
		return nil, err
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
			c.noteDead(ctx)
			err := Errorf(ErrRequestTimeout, "tool call timed out")
			c.observeFailure(ctx, start, version, toolName, err)
			return nil, err
		}
		if isResponseTooLarge(err) {
			err := Errorf(ErrMessageTooLarge, "tool result exceeds the configured message bound")
			c.observeFailure(ctx, start, version, toolName, err)
			return nil, err
		}
		c.noteDead(ctx)
		err := Wrap(ErrServerUnavailable, err, "tool call failed")
		c.observeFailure(ctx, start, version, toolName, err)
		return nil, err
	}
	c.mu.Lock()
	c.resetReconnectsLocked()
	c.mu.Unlock()
	var resultSize int64
	if raw, err := json.Marshal(res); err == nil {
		resultSize = int64(len(raw))
	}
	c.observe(ctx, &Observation{
		Server:          c.cfg.Name,
		Transport:       c.cfg.Transport,
		ProtocolVersion: version,
		Tool:            toolName,
		Result:          ResultToolCall,
		ResultCode:      ResultCodeOK,
		SizeBytes:       resultSize,
		Duration:        time.Since(start),
	})
	return res, nil
}

func (c *Client) Close(ctx context.Context) error {
	if ctx == nil {
		return Errorf(ErrConfigInvalid, "context must not be nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	var sessionErr error
	if c.session != nil {
		sessionErr = c.session.Close()
	}
	if c.streamCancel != nil {
		c.streamCancel()
	}
	if c.apiClient != nil {
		c.apiClient.CloseIdleConnections()
	}
	c.closeContainedLocked(ctx)
	return sessionErr
}

func (c *Client) ServerName() string {
	if c.cfg == nil {
		return ""
	}
	return c.cfg.Name
}

var discoveredToolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validateDiscoveredToolName(name string) error {
	if name == "" || len(name) > 64 {
		return Errorf(ErrSchemaInvalid, "tool name %q has invalid length", name)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return Errorf(ErrSchemaInvalid, "tool name %q contains control characters", name)
		}
	}
	if !discoveredToolNamePattern.MatchString(name) {
		return Errorf(ErrSchemaInvalid, "tool name %q uses illegal characters", name)
	}
	return nil
}

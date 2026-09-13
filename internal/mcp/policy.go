package mcp

import (
	"context"
	"net/http"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

type EndpointPolicy interface {
	ValidateEndpoint(ctx context.Context, rawURL string) error
}

type SecretResolver interface {
	ResolveSecret(ctx context.Context, ref string) (string, error)
}

type ClientOption func(*Client)

func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = client
	}
}

func WithEndpointPolicy(policy EndpointPolicy) ClientOption {
	return func(c *Client) {
		c.endpointPolicy = policy
	}
}

func WithSecretResolver(resolver SecretResolver) ClientOption {
	return func(c *Client) {
		c.secretResolver = resolver
	}
}

func WithOAuthCodeFetcher(fetch sdkauth.AuthorizationCodeFetcher) ClientOption {
	return func(c *Client) {
		c.oauthFetch = fetch
	}
}

func WithObserver(observer Observer) ClientOption {
	return func(c *Client) {
		c.observer = observer
	}
}

func WithSessionStarter(starter SessionStarter) ClientOption {
	return func(c *Client) {
		c.sessionStarter = starter
	}
}

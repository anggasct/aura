package embedding

import (
	"context"
	"net"
	"net/http"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/egress"
)

func refuseRedirect(_ *http.Request, _ []*http.Request) error {
	return Errorf(ErrorCodeRedirectRefused, "the embedding provider answered with a redirect, which is never followed")
}

func newRemoteClient(resolver egress.Resolver) *http.Client {
	client := egress.NewAPIClient(resolver)
	client.CheckRedirect = refuseRedirect
	return client
}

func newLoopbackClient(endpoint config.ServiceEndpoint) *http.Client {
	pinned := net.JoinHostPort(endpoint.Host, endpoint.Port)
	dialer := &net.Dialer{}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != pinned {
				return nil, Errorf(ErrorCodeDestinationDenied, "the embedding client dials only the configured loopback address")
			}
			return dialer.DialContext(ctx, network, pinned)
		},
	}
	return &http.Client{Transport: transport, CheckRedirect: refuseRedirect}
}

package knowledge

import (
	"net/http"

	"github.com/anggasct/aura/internal/platform/egress"
	"github.com/anggasct/aura/internal/platform/fault"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func refuseDeniedEgress(err error) error {
	if fault.HasCode(err, egress.ErrorCodeEgressDenied) {
		return Errorf(ErrorCodeURLRefused, "destination is not permitted")
	}
	return err
}

func NewHTTPClient(resolver egress.Resolver) *http.Client {
	client := egress.NewAPIClient(resolver)
	transport := client.Transport
	client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		response, err := transport.RoundTrip(request)
		return response, refuseDeniedEgress(err)
	})
	redirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		return refuseDeniedEgress(redirect(request, via))
	}
	return client
}

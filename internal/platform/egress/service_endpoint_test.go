package egress

import (
	"strings"
	"testing"
)

func TestParseServiceEndpointAcceptsValidEndpoints(t *testing.T) {
	tests := map[string]struct {
		raw      string
		want     ServiceEndpoint
		wantAuth string
		wantURL  string
	}{
		"remote https default port": {
			raw:      "https://API.Example.com/v1/",
			want:     ServiceEndpoint{Scheme: "https", Host: "api.example.com", Port: "443", Path: "/v1", Loopback: false},
			wantAuth: "api.example.com:443",
			wantURL:  "https://api.example.com:443/v1",
		},
		"remote https custom port": {
			raw:      "https://api.example.com:8443/v1/embed",
			want:     ServiceEndpoint{Scheme: "https", Host: "api.example.com", Port: "8443", Path: "/v1/embed", Loopback: false},
			wantAuth: "api.example.com:8443",
			wantURL:  "https://api.example.com:8443/v1/embed",
		},
		"ipv4 loopback http": {
			raw:      "http://127.0.0.1:11434/v1",
			want:     ServiceEndpoint{Scheme: "http", Host: "127.0.0.1", Port: "11434", Path: "/v1", Loopback: true},
			wantAuth: "127.0.0.1:11434",
			wantURL:  "http://127.0.0.1:11434/v1",
		},
		"ipv4 loopback https default port": {
			raw:      "https://127.0.0.1/v1",
			want:     ServiceEndpoint{Scheme: "https", Host: "127.0.0.1", Port: "443", Path: "/v1", Loopback: true},
			wantAuth: "127.0.0.1:443",
			wantURL:  "https://127.0.0.1:443/v1",
		},
		"ipv6 loopback http": {
			raw:      "http://[::1]:11434/v1",
			want:     ServiceEndpoint{Scheme: "http", Host: "::1", Port: "11434", Path: "/v1", Loopback: true},
			wantAuth: "[::1]:11434",
			wantURL:  "http://[::1]:11434/v1",
		},
		"ipv4 loopback default http port": {
			raw:      "http://127.0.0.1",
			want:     ServiceEndpoint{Scheme: "http", Host: "127.0.0.1", Port: "80", Path: "", Loopback: true},
			wantAuth: "127.0.0.1:80",
			wantURL:  "http://127.0.0.1:80",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseServiceEndpoint(tc.raw)
			if err != nil {
				t.Fatalf("ParseServiceEndpoint(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("endpoint = %+v, want %+v", got, tc.want)
			}
			if got.Authority() != tc.wantAuth {
				t.Errorf("Authority() = %q, want %q", got.Authority(), tc.wantAuth)
			}
			if got.URL() != tc.wantURL {
				t.Errorf("URL() = %q, want %q", got.URL(), tc.wantURL)
			}
		})
	}
}

func TestParseServiceEndpointRejectsLoopbackLookalikes(t *testing.T) {
	for _, raw := range []string{
		"http://localhost:11434/v1",
		"https://localhost/v1",
		"http://LOCALHOST/v1",
		"http://localhost./v1",
		"http://embed.localhost/v1",
		"http://127.0.0.2/v1",
		"http://127.1/v1",
		"http://[::ffff:127.0.0.1]/v1",
		"http://127.0.0.1./v1",
		"http://[0:0:0:0:0:0:0:1]/v1",
		"http://2130706433/v1",
		"http://0x7f.0.0.1/v1",
		"https://127.0.0.2/v1",
		"https://[::ffff:7f00:1]/v1",
		"http://[::1%25lo]/v1",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseServiceEndpoint(raw); err == nil {
				t.Fatalf("ParseServiceEndpoint(%q) succeeded, want error", raw)
			}
		})
	}
}

func TestParseServiceEndpointRejectsInvalidInputs(t *testing.T) {
	tests := map[string]struct {
		raw    string
		detail string
	}{
		"empty":                {raw: "", detail: "must be set"},
		"surrounding space":    {raw: " https://api.example.com/v1 ", detail: "surrounding whitespace"},
		"ftp scheme":           {raw: "ftp://api.example.com/v1", detail: "scheme must be http or https"},
		"missing host":         {raw: "https:///v1", detail: "must have a host"},
		"embedded credentials": {raw: "https://user:pw@api.example.com/v1", detail: "must not embed credentials"},
		"query string":         {raw: "https://api.example.com/v1?key=1", detail: "query or fragment"},
		"empty query":          {raw: "https://api.example.com/v1?", detail: "query or fragment"},
		"fragment":             {raw: "https://api.example.com/v1#x", detail: "query or fragment"},
		"plain http remote":    {raw: "http://api.example.com/v1", detail: "must use https"},
		"private literal":      {raw: "https://10.0.0.5/v1", detail: "not a permitted destination"},
		"metadata literal":     {raw: "https://169.254.169.254/v1", detail: "not a permitted destination"},
		"port zero":            {raw: "https://api.example.com:0/v1", detail: "port must be between"},
		"port above max":       {raw: "https://api.example.com:65536/v1", detail: "port must be between"},
		"leading zero port":    {raw: "https://api.example.com:0443/v1", detail: "port must be between"},
		"dot segment":          {raw: "https://api.example.com/a/../v1", detail: "path must be plain"},
		"encoded path":         {raw: "https://api.example.com/a%2Fb", detail: "path must be plain"},
		"double slash path":    {raw: "https://api.example.com/a//b", detail: "path must be plain"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseServiceEndpoint(tc.raw)
			if err == nil || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("ParseServiceEndpoint(%q) error = %v, want %q", tc.raw, err, tc.detail)
			}
		})
	}
}

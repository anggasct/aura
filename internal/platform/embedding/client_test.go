package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/platform/egress"
)

func testConfig(baseURL string) *config.KnowledgeEmbedding {
	return &config.KnowledgeEmbedding{
		BaseURL: baseURL, Model: "test-model", Dimensions: 3, DimensionsField: "dimensions",
		RoleStyle: config.EmbeddingRoleNone, BatchSize: 96, MaxMemory: config.DefaultEmbeddingMaxMemory,
	}
}

type recordedRequest struct {
	header http.Header
	body   map[string]any
	path   string
}

func embeddingServer(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func vectorsJSON(vectors ...string) string {
	items := make([]string, 0, len(vectors))
	for i, vector := range vectors {
		items = append(items, `{"object":"embedding","index":`+strconv.Itoa(i)+`,"embedding":`+vector+`}`)
	}
	return `{"object":"list","data":[` + strings.Join(items, ",") + `],"usage":{"total_tokens":4}}`
}

func respondJSON(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

func TestClientSendsTheConfiguredRequestShapes(t *testing.T) {
	tests := map[string]struct {
		mutate   func(*config.KnowledgeEmbedding)
		role     Role
		wantBody map[string]any
	}{
		"plain document": {
			mutate:   func(*config.KnowledgeEmbedding) {},
			role:     RoleDocument,
			wantBody: map[string]any{"model": "test-model", "dimensions": float64(3), "input": []any{"alpha", "beta"}},
		},
		"custom dimensions field": {
			mutate:   func(c *config.KnowledgeEmbedding) { c.DimensionsField = "output_dimension" },
			role:     RoleDocument,
			wantBody: map[string]any{"model": "test-model", "output_dimension": float64(3), "input": []any{"alpha", "beta"}},
		},
		"input type document": {
			mutate:   func(c *config.KnowledgeEmbedding) { c.RoleStyle = config.EmbeddingRoleInputType },
			role:     RoleDocument,
			wantBody: map[string]any{"model": "test-model", "dimensions": float64(3), "input": []any{"alpha", "beta"}, "input_type": "document"},
		},
		"input type query": {
			mutate:   func(c *config.KnowledgeEmbedding) { c.RoleStyle = config.EmbeddingRoleInputType },
			role:     RoleQuery,
			wantBody: map[string]any{"model": "test-model", "dimensions": float64(3), "input": []any{"alpha", "beta"}, "input_type": "query"},
		},
		"prefix document": {
			mutate: func(c *config.KnowledgeEmbedding) {
				c.RoleStyle, c.QueryPrefix, c.DocumentPrefix = config.EmbeddingRolePrefix, "Q: ", "D: "
			},
			role:     RoleDocument,
			wantBody: map[string]any{"model": "test-model", "dimensions": float64(3), "input": []any{"D: alpha", "D: beta"}},
		},
		"prefix query": {
			mutate: func(c *config.KnowledgeEmbedding) {
				c.RoleStyle, c.QueryPrefix, c.DocumentPrefix = config.EmbeddingRolePrefix, "Q: ", "D: "
			},
			role:     RoleQuery,
			wantBody: map[string]any{"model": "test-model", "dimensions": float64(3), "input": []any{"Q: alpha", "Q: beta"}},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var seen recordedRequest
			server, _ := embeddingServer(t, func(w http.ResponseWriter, r *http.Request) {
				seen.path = r.URL.Path
				seen.header = r.Header.Clone()
				_ = json.NewDecoder(r.Body).Decode(&seen.body)
				respondJSON(vectorsJSON("[1,2,3]", "[0,1,0]"))(w, r)
			})
			cfg := testConfig(server.URL + "/v1")
			test.mutate(cfg)
			client, err := New(cfg, "", nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			vectors, err := client.Embed(t.Context(), test.role, []string{"alpha", "beta"})
			if err != nil || len(vectors) != 2 || vectors[0][2] != 3 {
				t.Fatalf("Embed = %v, %v", vectors, err)
			}
			if seen.path != "/v1/embeddings" {
				t.Errorf("path = %q", seen.path)
			}
			encodedWant, _ := json.Marshal(test.wantBody)
			encodedGot, _ := json.Marshal(seen.body)
			if !bytes.Equal(encodedWant, encodedGot) {
				t.Errorf("body = %s, want %s", encodedGot, encodedWant)
			}
			if seen.header.Get("Authorization") != "" {
				t.Errorf("Authorization = %q without a credential", seen.header.Get("Authorization"))
			}
		})
	}
}

func TestClientSendsTheCredentialAsABearerToken(t *testing.T) {
	var authorization string
	server, _ := embeddingServer(t, func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		respondJSON(vectorsJSON("[1,0,0]"))(w, r)
	})
	client, err := New(testConfig(server.URL), "sk-test-credential", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Embed(t.Context(), RoleDocument, []string{"alpha"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if authorization != "Bearer sk-test-credential" {
		t.Errorf("Authorization = %q", authorization)
	}
}

func TestClientRejectsMalformedResponses(t *testing.T) {
	tests := map[string]struct {
		body string
		want ErrorCode
	}{
		"fewer vectors":       {body: vectorsJSON("[1,0,0]"), want: ErrorCodeResponseInvalid},
		"more vectors":        {body: vectorsJSON("[1,0,0]", "[0,1,0]", "[0,0,1]"), want: ErrorCodeResponseInvalid},
		"short vector":        {body: vectorsJSON("[1,0,0]", "[1,0]"), want: ErrorCodeResponseInvalid},
		"long vector":         {body: vectorsJSON("[1,0,0]", "[1,0,0,1]"), want: ErrorCodeResponseInvalid},
		"all zero":            {body: vectorsJSON("[1,0,0]", "[0,0,0]"), want: ErrorCodeResponseInvalid},
		"overflowing value":   {body: vectorsJSON("[1,0,0]", "[1e39,0,0]"), want: ErrorCodeResponseInvalid},
		"unparseable number":  {body: vectorsJSON("[1,0,0]", "[1e999,0,0]"), want: ErrorCodeResponseInvalid},
		"string value":        {body: vectorsJSON("[1,0,0]", `["x",0,0]`), want: ErrorCodeResponseInvalid},
		"base64 embedding":    {body: vectorsJSON("[1,0,0]", `"AAAA"`), want: ErrorCodeResponseInvalid},
		"null embedding":      {body: vectorsJSON("[1,0,0]", `null`), want: ErrorCodeResponseInvalid},
		"out of order":        {body: `{"data":[{"index":1,"embedding":[1,0,0]},{"index":0,"embedding":[0,1,0]}]}`, want: ErrorCodeResponseInvalid},
		"trailing content":    {body: vectorsJSON("[1,0,0]", "[0,1,0]") + ` {}`, want: ErrorCodeResponseInvalid},
		"not json":            {body: `<html>`, want: ErrorCodeResponseInvalid},
		"empty object":        {body: `{}`, want: ErrorCodeResponseInvalid},
		"missing data":        {body: `{"error":{"message":"bad"}}`, want: ErrorCodeResponseInvalid},
		"empty body":          {body: ``, want: ErrorCodeResponseInvalid},
		"oversized body":      {body: `{"data":[],"pad":"` + strings.Repeat("a", 2<<20) + `"}`, want: ErrorCodeResponseInvalid},
		"unindexed but valid": {body: `{"data":[{"embedding":[1,0,0]},{"embedding":[0,1,0]}]}`, want: ""},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			server, _ := embeddingServer(t, respondJSON(test.body))
			client, err := New(testConfig(server.URL), "", nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			vectors, err := client.Embed(t.Context(), RoleDocument, []string{"alpha", "beta"})
			if test.want == "" {
				if err != nil || len(vectors) != 2 {
					t.Fatalf("Embed = %v, %v; want success", vectors, err)
				}
				return
			}
			if code, ok := CodeOf(err); !ok || code != test.want || vectors != nil {
				t.Fatalf("Embed = %v, %v; want code %s and no vectors", vectors, err, test.want)
			}
		})
	}
}

func TestClientReportsProviderStatusWithoutTheBody(t *testing.T) {
	server, _ := embeddingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid key sk-leaked-secret"}`)
	})
	client, err := New(testConfig(server.URL), "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Embed(t.Context(), RoleDocument, []string{"alpha"})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeStatusRejected || strings.Contains(err.Error(), "sk-leaked-secret") {
		t.Fatalf("error = %v, want a status error without the provider body", err)
	}
}

func TestClientRejectsInvalidRequestsBeforeSending(t *testing.T) {
	server, calls := embeddingServer(t, respondJSON(vectorsJSON("[1,0,0]")))
	client, err := New(testConfig(server.URL), "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tooMany := make([]string, config.MaxEmbeddingBatchSize+1)
	for i := range tooMany {
		tooMany[i] = "x"
	}
	for name, test := range map[string]struct {
		role  Role
		texts []string
	}{
		"no texts":     {role: RoleDocument},
		"empty text":   {role: RoleDocument, texts: []string{""}},
		"unknown role": {role: Role("other"), texts: []string{"a"}},
		"too many":     {role: RoleDocument, texts: tooMany},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := client.Embed(t.Context(), test.role, test.texts)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeRequestInvalid {
				t.Fatalf("error = %v, want %s", err, ErrorCodeRequestInvalid)
			}
		})
	}
	if calls.Load() != 0 {
		t.Errorf("provider calls = %d, want none", calls.Load())
	}
}

func TestClientRefusesEveryRedirect(t *testing.T) {
	for name, status := range map[string]int{"found": http.StatusFound, "temporary": http.StatusTemporaryRedirect, "permanent": http.StatusPermanentRedirect} {
		t.Run(name, func(t *testing.T) {
			var follow atomic.Int32
			server, calls := embeddingServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/followed" {
					follow.Add(1)
					respondJSON(vectorsJSON("[1,0,0]"))(w, r)
					return
				}
				http.Redirect(w, r, "/v1/followed", status)
			})
			client, err := New(testConfig(server.URL+"/v1"), "sk-test-credential", nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = client.Embed(t.Context(), RoleDocument, []string{"alpha"})
			if code, ok := CodeOf(err); !ok || code != ErrorCodeRedirectRefused {
				t.Fatalf("error = %v, want %s", err, ErrorCodeRedirectRefused)
			}
			if follow.Load() != 0 || calls.Load() != 1 {
				t.Errorf("redirect followed: target calls %d, total calls %d", follow.Load(), calls.Load())
			}
		})
	}
}

func TestRemoteClientRefusesSameOriginRedirects(t *testing.T) {
	client, err := New(testConfig("https://api.example.com/v1"), "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.example.com/v1/embeddings", http.NoBody)
	next, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.example.com/v1/other", http.NoBody)
	err = client.http.CheckRedirect(next, []*http.Request{first})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeRedirectRefused {
		t.Fatalf("CheckRedirect = %v, want %s", err, ErrorCodeRedirectRefused)
	}
}

type loopbackResolver struct{}

func (loopbackResolver) LookupIP(context.Context, string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("127.0.0.1")}, nil
}

func TestRemoteClientDeniesLoopbackAndPrivateDestinations(t *testing.T) {
	var reached atomic.Int32
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			reached.Add(1)
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	client, err := New(testConfig("https://embed.example.com:"+port+"/v1"), "", loopbackResolver{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Embed(t.Context(), RoleDocument, []string{"alpha"})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeDestinationDenied {
		t.Fatalf("error = %v, want %s", err, ErrorCodeDestinationDenied)
	}
	if code, ok := egress.CodeOf(err); !ok || code != egress.ErrorCodeEgressDenied {
		t.Errorf("egress code = %v, %v", code, ok)
	}
	if reached.Load() != 0 {
		t.Errorf("loopback listener received %d connections through the remote client", reached.Load())
	}
}

func TestNewRejectsAnUnusableEndpoint(t *testing.T) {
	for _, baseURL := range []string{
		"http://api.example.com/v1",
		"http://localhost:11434/v1",
		"http://127.0.0.2/v1",
		"http://[::ffff:127.0.0.1]/v1",
		"http://127.0.0.1./v1",
		"",
	} {
		t.Run(baseURL, func(t *testing.T) {
			_, err := New(testConfig(baseURL), "", nil)
			if code, ok := CodeOf(err); !ok || code != ErrorCodeConfigInvalid {
				t.Fatalf("New = %v, want %s", err, ErrorCodeConfigInvalid)
			}
		})
	}
	if _, err := New(nil, "", nil); err == nil {
		t.Fatal("New accepted nil configuration")
	}
}

func TestLoopbackClientIsPinnedAndHasNoProxy(t *testing.T) {
	client, err := New(testConfig("http://127.0.0.1:11434/v1"), "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", client.http.Transport)
	}
	if transport.Proxy != nil {
		t.Error("the loopback transport consults a proxy")
	}
	for _, address := range []string{"127.0.0.1:11435", "127.0.0.2:11434", "localhost:11434", "10.0.0.1:11434", "[::1]:11434"} {
		conn, err := transport.DialContext(t.Context(), "tcp", address)
		if conn != nil {
			_ = conn.Close()
		}
		if code, ok := CodeOf(err); !ok || code != ErrorCodeDestinationDenied {
			t.Errorf("dial %s = %v, want %s", address, err, ErrorCodeDestinationDenied)
		}
	}
}

func TestLoopbackClientDialsOnlyTheConfiguredAddress(t *testing.T) {
	server, calls := embeddingServer(t, respondJSON(vectorsJSON("[1,0,0]")))
	client, err := New(testConfig(server.URL), "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Embed(t.Context(), RoleDocument, []string{"alpha"}); err != nil || calls.Load() != 1 {
		t.Fatalf("Embed = %v, calls %d", err, calls.Load())
	}
}

func TestClientStopsWhenTheProviderIsSlow(t *testing.T) {
	server, _ := embeddingServer(t, func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	})
	client, err := New(testConfig(server.URL), "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client.timeout = 50 * time.Millisecond
	_, err = client.Embed(t.Context(), RoleDocument, []string{"alpha"})
	if code, ok := CodeOf(err); !ok || code != ErrorCodeRequestFailed {
		t.Fatalf("error = %v, want %s", err, ErrorCodeRequestFailed)
	}
}

func TestFingerprintBindsEverythingThatChangesTheVectorSpace(t *testing.T) {
	base := func() *config.KnowledgeEmbedding {
		cfg := testConfig("https://api.example.com/v1")
		cfg.RoleStyle, cfg.QueryPrefix, cfg.DocumentPrefix = config.EmbeddingRolePrefix, "q ", "d "
		return cfg
	}
	reference, err := Fingerprint(base())
	if err != nil || len(reference) != 64 {
		t.Fatalf("Fingerprint = %q, %v", reference, err)
	}
	if again, _ := Fingerprint(base()); again != reference {
		t.Fatal("Fingerprint is not stable")
	}
	changes := map[string]func(*config.KnowledgeEmbedding){
		"host":             func(c *config.KnowledgeEmbedding) { c.BaseURL = "https://api.other.example/v1" },
		"port":             func(c *config.KnowledgeEmbedding) { c.BaseURL = "https://api.example.com:8443/v1" },
		"path":             func(c *config.KnowledgeEmbedding) { c.BaseURL = "https://api.example.com/v2" },
		"model":            func(c *config.KnowledgeEmbedding) { c.Model = "other-model" },
		"dimensions":       func(c *config.KnowledgeEmbedding) { c.Dimensions = 4 },
		"dimensions field": func(c *config.KnowledgeEmbedding) { c.DimensionsField = "output_dimension" },
		"role style": func(c *config.KnowledgeEmbedding) {
			c.RoleStyle = config.EmbeddingRoleNone
			c.QueryPrefix, c.DocumentPrefix = "", ""
		},
		"query prefix":    func(c *config.KnowledgeEmbedding) { c.QueryPrefix = "q2 " },
		"document prefix": func(c *config.KnowledgeEmbedding) { c.DocumentPrefix = "d2 " },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			change(cfg)
			changed, err := Fingerprint(cfg)
			if err != nil || changed == reference {
				t.Fatalf("Fingerprint after %s change = %q, %v; want a different value", name, changed, err)
			}
		})
	}
	same := map[string]func(*config.KnowledgeEmbedding){
		"batch size":     func(c *config.KnowledgeEmbedding) { c.BatchSize = 7 },
		"credential":     func(c *config.KnowledgeEmbedding) { c.CredentialRef = "env://OTHER" },
		"memory":         func(c *config.KnowledgeEmbedding) { c.MaxMemory = 1 << 30 },
		"public queries": func(c *config.KnowledgeEmbedding) { c.PublicQueries = true },
		"trailing slash": func(c *config.KnowledgeEmbedding) { c.BaseURL = "https://API.example.com/v1/" },
		"default port":   func(c *config.KnowledgeEmbedding) { c.BaseURL = "https://api.example.com:443/v1" },
	}
	for name, change := range same {
		t.Run("keeps "+name, func(t *testing.T) {
			cfg := base()
			change(cfg)
			if got, _ := Fingerprint(cfg); got != reference {
				t.Fatalf("Fingerprint after %s change differs", name)
			}
		})
	}
	if _, err := Fingerprint(testConfig("http://localhost/v1")); err == nil {
		t.Fatal("Fingerprint accepted an invalid endpoint")
	}
}

func TestFingerprintSeparatesAdjacentFields(t *testing.T) {
	left := testConfig("https://api.example.com/v1")
	left.Model = "ab"
	left.DimensionsField = "cd"
	right := testConfig("https://api.example.com/v1")
	right.Model = "a"
	right.DimensionsField = "bcd"
	first, _ := Fingerprint(left)
	second, _ := Fingerprint(right)
	if first == second {
		t.Fatal("adjacent fields collide")
	}
}

package toolbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/approval"
	"github.com/anggasct/aura/internal/egress"
	"github.com/anggasct/aura/internal/store"
)

func oversizedAdapter(output json.RawMessage) map[string]Adapter {
	return map[string]Adapter{
		"read_file@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			return ToolResult{Output: output}, nil
		},
	}
}

func oversizedOutput(size int) json.RawMessage {
	return json.RawMessage(`{"content":"` + strings.Repeat("a", size) + `"}`)
}

func TestBrokerMapsEgressDenialToPolicyDenied(t *testing.T) {
	broker, err := New(&Options{Adapters: map[string]Adapter{
		"web_fetch@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
			return ToolResult{}, egress.Errorf(egress.ErrorCodeEgressDenied, "loopback address 127.0.0.1")
		},
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = broker.Execute(context.Background(), brokerRequest("web_fetch", `{"url":"https://127.0.0.1/doc"}`, "public-web"))
	if class := classOf(err); class != ResultPolicyDenied {
		t.Fatalf("class = %q, want policy_denied (%v)", class, err)
	}
}

func TestBrokerPreservesAdapterResultClasses(t *testing.T) {
	cases := []struct {
		name        string
		tool        string
		arguments   string
		capability  string
		adapterErr  error
		want        ResultClass
		wantSegment string
	}{
		{
			name:        "filesystem policy denial",
			tool:        "read_file",
			arguments:   `{"path":"../secret"}`,
			capability:  "workspace-read",
			adapterErr:  errorf(ResultPolicyDenied, "read path is outside the workspace"),
			want:        ResultPolicyDenied,
			wantSegment: "outside the workspace",
		},
		{
			name:        "fetch policy denial",
			tool:        "web_fetch",
			arguments:   `{"url":"https://public.example/doc"}`,
			capability:  "public-web",
			adapterErr:  errorf(ResultPolicyDenied, "response exceeds encoded body limit"),
			want:        ResultPolicyDenied,
			wantSegment: "encoded body limit",
		},
		{
			name:        "missing search credentials",
			tool:        "web_search",
			arguments:   `{"query":"aura"}`,
			capability:  "provider-search",
			adapterErr:  errorf(ResultCapabilityUnavailable, "search credential is unavailable"),
			want:        ResultCapabilityUnavailable,
			wantSegment: "credential is unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker, err := New(&Options{
				Adapters: map[string]Adapter{
					tc.tool + "@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
						return ToolResult{}, tc.adapterErr
					},
				},
				Secrets: []string{"search-secret"},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			request := brokerRequest(tc.tool, tc.arguments, tc.capability)
			request.RequestID = "request-" + tc.tool
			request.IdempotencyKey = "idempotency-" + tc.tool
			_, err = broker.Execute(context.Background(), request)
			if class := classOf(err); class != tc.want {
				t.Fatalf("class = %q, want %q (err = %v)", class, tc.want, err)
			}
			if !strings.Contains(err.Error(), tc.wantSegment) {
				t.Fatalf("error detail %q lost the adapter message: %v", tc.wantSegment, err)
			}
		})
	}
}

func TestBrokerRedactsAdapterErrorDetail(t *testing.T) {
	broker, err := New(&Options{
		Adapters: map[string]Adapter{
			"web_search@v1": func(context.Context, *ToolRequest, approval.Constraints) (ToolResult, error) {
				return ToolResult{}, errorf(ResultExecutionFailed, "provider rejected token search-secret")
			},
		},
		Secrets: []string{"search-secret"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = broker.Execute(context.Background(), brokerRequest("web_search", `{"query":"aura"}`, "provider-search"))
	if class := classOf(err); class != ResultExecutionFailed {
		t.Fatalf("class = %q, err = %v", class, err)
	}
	if strings.Contains(err.Error(), "search-secret") {
		t.Fatalf("adapter error leaked a secret: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error detail was not redacted: %v", err)
	}
}

func TestBrokerOversizedOutputReturnsDecodableTruncationEnvelope(t *testing.T) {
	output := oversizedOutput(8192)
	broker, err := New(&Options{Adapters: oversizedAdapter(output), MaxInlineResultBytes: 4096})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := broker.Execute(context.Background(), brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Truncated {
		t.Fatalf("result.Truncated = false, want true")
	}
	if int64(len(result.Output)) > 4096 {
		t.Fatalf("bounded output length = %d, want <= 4096", len(result.Output))
	}
	var envelope boundedOutput
	if err := json.Unmarshal(result.Output, &envelope); err != nil {
		t.Fatalf("bounded output is not decodable JSON: %v (%s)", err, result.Output)
	}
	digest := sha256.Sum256(output)
	if envelope.Digest != hex.EncodeToString(digest[:]) {
		t.Fatalf("digest = %q, want sha256 of the full output", envelope.Digest)
	}
	if envelope.SizeBytes != int64(len(output)) || !envelope.Truncated || envelope.ArtifactID != "" {
		t.Fatalf("envelope = %+v", envelope)
	}
	if envelope.Body == "" {
		t.Fatal("inline body is empty")
	}
	if !strings.HasPrefix(string(output), envelope.Body) {
		t.Fatal("inline body is not a prefix of the original output")
	}
}

func TestBrokerOversizedOutputSpillsToArtifact(t *testing.T) {
	output := oversizedOutput(8192)
	db, err := store.OpenDB(context.Background(), filepath.Join(t.TempDir(), "aura.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sessions := store.NewSessionService(db)
	if err := sessions.Create(context.Background(), &store.Session{ID: "session-1", OwnerID: "owner-1", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	artifacts := store.NewArtifactStore(db, filepath.Join(t.TempDir(), "artifacts"), 1<<20)
	broker, err := New(&Options{Adapters: oversizedAdapter(output), MaxInlineResultBytes: 4096, Artifacts: artifacts})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := broker.Execute(context.Background(), brokerRequest("read_file", `{"path":"note.txt"}`, "workspace-read"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.Truncated {
		t.Fatalf("result.Truncated = false, want true (inline output is bounded)")
	}
	var envelope boundedOutput
	if err := json.Unmarshal(result.Output, &envelope); err != nil {
		t.Fatalf("bounded output is not decodable JSON: %v", err)
	}
	if envelope.ArtifactID == "" || envelope.Truncated || envelope.Body != "" {
		t.Fatalf("envelope = %+v, want artifact reference without inline body", envelope)
	}
	if envelope.SizeBytes != int64(len(output)) {
		t.Fatalf("size = %d, want %d", envelope.SizeBytes, len(output))
	}
	reader, _, err := artifacts.Open(context.Background(), envelope.ArtifactID)
	if err != nil {
		t.Fatalf("open artifact: %v", err)
	}
	defer func() { _ = reader.Close() }()
	spilled, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(spilled) != string(output) {
		t.Fatalf("artifact content length = %d, want the full %d-byte output", len(spilled), len(output))
	}
	digest := sha256.Sum256(output)
	if envelope.Digest != hex.EncodeToString(digest[:]) {
		t.Fatalf("digest = %q, want sha256 of the full output", envelope.Digest)
	}
}

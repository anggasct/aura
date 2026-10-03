package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/anggasct/aura/internal/config"
)

func newVisionCaptureServer(t *testing.T, captured *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		*captured = body
		writeFixture(t, w, fixtureBytes(t, "openai_completion.json"))
	}))
}

type fakeBlobReader struct {
	blobs map[string][]byte
	calls int
	err   error
}

func (f *fakeBlobReader) ReadBlob(ctx context.Context, refID string, maxBytes int64) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	data, ok := f.blobs[refID]
	if !ok {
		return nil, errors.New("blob is missing")
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("blob exceeds the read bound")
	}
	return data, nil
}

func testVisionConfig() *config.Vision {
	return &config.Vision{
		Enabled:              true,
		MaxEncodedBytes:      20971520,
		MaxPixels:            40000000,
		MaxDimension:         8192,
		MaxImages:            4,
		MaxFrames:            1,
		DecodeTimeout:        config.Duration(5 * time.Second),
		MaxDecodeConcurrency: 2,
		MaxRequestBytes:      33554432,
		StripMetadata:        true,
		OrientNormalize:      true,
		Detail:               "auto",
		TransformVersion:     "v1",
		MaxTransformMemory:   config.ByteSize(268435456),
		Providers:            map[string]config.VisionProviderLimits{},
	}
}

func visionDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func visionEnvelopeJSON(refID string, data []byte, mime string, width, height int) string {
	raw, err := json.Marshal(map[string]any{
		"kind":              "image_ref.v1",
		"artifact_id":       refID,
		"source_digest":     visionDigest(data),
		"derived_digest":    visionDigest(data),
		"mime":              mime,
		"width":             width,
		"height":            height,
		"encoded_bytes":     len(data),
		"transform_version": "v1",
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func visionFilePart(refID string, data []byte, mime string, width, height int) *genai.Part {
	return &genai.Part{
		FileData: &genai.FileData{
			FileURI:  "artifact://" + refID,
			MIMEType: mime,
		},
		PartMetadata: map[string]any{"image_ref": visionEnvelopeJSON(refID, data, mime, width, height)},
	}
}

func visionRequest(parts ...*genai.Part) *adkmodel.LLMRequest {
	return &adkmodel.LLMRequest{
		Model:    "test-model",
		Contents: []*genai.Content{{Role: "user", Parts: parts}},
	}
}

func TestVisionPolicyDisabled(t *testing.T) {
	if NewVisionPolicy(nil) != nil {
		t.Fatal("nil config must yield nil policy")
	}
	disabled := testVisionConfig()
	disabled.Enabled = false
	if NewVisionPolicy(disabled) != nil {
		t.Fatal("disabled config must yield nil policy")
	}
}

func TestVisionPolicyProviderNarrowing(t *testing.T) {
	cfg := testVisionConfig()
	cfg.Providers["openai_chat_compat"] = config.VisionProviderLimits{MaxImages: 2, Detail: "low"}
	policy := NewVisionPolicy(cfg)
	if policy == nil {
		t.Fatal("policy is nil")
	}
	if got := policy.DetailFor("openai_chat_compat"); got != "low" {
		t.Fatalf("DetailFor = %q, want low", got)
	}
	if got := policy.DetailFor("gemini_native"); got != "auto" {
		t.Fatalf("DetailFor = %q, want auto", got)
	}
	images := []VisionImage{
		{MIME: "image/png", Width: 8, Height: 8, EncodedBytes: 100},
		{MIME: "image/png", Width: 8, Height: 8, EncodedBytes: 100},
		{MIME: "image/png", Width: 8, Height: 8, EncodedBytes: 100},
	}
	if err := policy.CheckCandidate("gemini_native", images); err != nil {
		t.Fatalf("CheckCandidate gemini: %v", err)
	}
	if err := policy.CheckCandidate("openai_chat_compat", images); err == nil {
		t.Fatal("expected provider image-count rejection")
	} else if code, _ := CodeOf(err); code != ErrorCodeVisionBudgetExceeded {
		t.Fatalf("code = %q, want vision_budget_exceeded", code)
	}
}

func TestVisionPolicyBudgetGates(t *testing.T) {
	policy := NewVisionPolicy(testVisionConfig())
	big := VisionImage{MIME: "image/png", Width: 8, Height: 8, EncodedBytes: 20971521}
	if err := policy.CheckGlobal([]VisionImage{big}); err == nil {
		t.Fatal("expected per-image budget rejection")
	}
	many := make([]VisionImage, 5)
	for i := range many {
		many[i] = VisionImage{MIME: "image/png", Width: 8, Height: 8, EncodedBytes: 64}
	}
	if err := policy.CheckGlobal(many); err == nil {
		t.Fatal("expected image-count rejection")
	}
	badGeometry := VisionImage{MIME: "image/png", Width: 9000, Height: 8, EncodedBytes: 64}
	if err := policy.CheckGlobal([]VisionImage{badGeometry}); err == nil {
		t.Fatal("expected geometry rejection")
	} else if code, _ := CodeOf(err); code != ErrorCodeProtocolInvalid {
		t.Fatalf("code = %q, want model_protocol_invalid", code)
	}
}

func TestPredicateForRequestDetectsFileDataImage(t *testing.T) {
	data := []byte("fake-png-bytes")
	req := visionRequest(visionFilePart("art-1", data, "image/png", 8, 8))
	if !PredicateForRequest(req).Vision {
		t.Fatal("FileData image reference must require the vision capability")
	}
	if !PredicateVisionImages(req) {
		t.Fatal("PredicateVisionImages must detect the file reference")
	}
	plain := visionRequest(&genai.Part{Text: "hello"})
	if PredicateVisionImages(plain) {
		t.Fatal("text-only request must not flag vision input")
	}
}

func TestCollectVisionImagesRejects(t *testing.T) {
	data := []byte("fake-png-bytes")
	cases := map[string]*genai.Part{
		"missing envelope": {FileData: &genai.FileData{FileURI: "artifact://art-1", MIMEType: "image/png"}},
		"foreign uri":      {FileData: &genai.FileData{FileURI: "https://example.com/x.png", MIMEType: "image/png"}},
		"bad ref id":       {FileData: &genai.FileData{FileURI: "artifact://../escape", MIMEType: "image/png"}, PartMetadata: map[string]any{"image_ref": visionEnvelopeJSON("art-1", data, "image/png", 8, 8)}},
		"mime mismatch":    {FileData: &genai.FileData{FileURI: "artifact://art-1", MIMEType: "image/jpeg"}, PartMetadata: map[string]any{"image_ref": visionEnvelopeJSON("art-1", data, "image/png", 8, 8)}},
		"bad kind":         {FileData: &genai.FileData{FileURI: "artifact://art-1", MIMEType: "image/png"}, PartMetadata: map[string]any{"image_ref": `{"kind":"other.v9","artifact_id":"art-1","derived_digest":"sha256:aa","mime":"image/png","width":8,"height":8,"encoded_bytes":3,"transform_version":"v1"}`}},
	}
	for name, part := range cases {
		t.Run(name, func(t *testing.T) {
			req := visionRequest(part)
			if _, err := collectVisionImages(req.Contents); err == nil {
				t.Fatal("expected rejection")
			} else if code, _ := CodeOf(err); code != ErrorCodeProtocolInvalid {
				t.Fatalf("code = %q, want model_protocol_invalid", code)
			}
		})
	}
}

func resolveTestClient(policy *VisionPolicy, blobs VisionBlobReader, protocol string) *coreClient {
	var codec providerCodec = openaiCodec{}
	switch protocol {
	case "anthropic_messages":
		codec = anthropicCodec{}
	case "openai_responses":
		codec = openAIResponsesCodec{}
	case "gemini_native":
		codec = geminiCodec{}
	}
	return newCoreClient(nil, "test", "http://127.0.0.1:9", "", 0, 0, codec).withVision(visionWiring{policy: policy, blobs: blobs})
}

func TestResolveVisionImagesReplacesWithInlineData(t *testing.T) {
	data := []byte("resolved-image-bytes")
	blobs := &fakeBlobReader{blobs: map[string][]byte{"art-1": data}}
	client := resolveTestClient(NewVisionPolicy(testVisionConfig()), blobs, "openai_chat_compat")
	req := visionRequest(&genai.Part{Text: "look"}, visionFilePart("art-1", data, "image/png", 8, 8), &genai.Part{Text: "now"})
	if err := client.resolveVisionImages(context.Background(), req); err != nil {
		t.Fatalf("resolveVisionImages: %v", err)
	}
	if blobs.calls != 1 {
		t.Fatalf("blob reads = %d, want 1", blobs.calls)
	}
	imagePart := req.Contents[0].Parts[1]
	if imagePart.FileData != nil {
		t.Fatal("file reference must be replaced")
	}
	if imagePart.InlineData == nil || !bytes.Equal(imagePart.InlineData.Data, data) {
		t.Fatal("resolved bytes are missing")
	}
	if imagePart.InlineData.MIMEType != "image/png" {
		t.Fatalf("mime = %q", imagePart.InlineData.MIMEType)
	}
	if req.Contents[0].Parts[0].Text != "look" || req.Contents[0].Parts[2].Text != "now" {
		t.Fatal("part order must be preserved")
	}
}

func TestResolveVisionImagesFailsClosed(t *testing.T) {
	data := []byte("resolved-image-bytes")
	other := []byte("tampered-bytes")
	policy := NewVisionPolicy(testVisionConfig())
	cases := map[string]struct {
		blobs   *fakeBlobReader
		part    *genai.Part
		policy  *VisionPolicy
		want    ErrorCode
		network bool
	}{
		"digest mismatch": {blobs: &fakeBlobReader{blobs: map[string][]byte{"art-1": other}}, part: visionFilePart("art-1", data, "image/png", 8, 8), policy: policy, want: ErrorCodeVisionArtifactUnavailable},
		"missing blob":    {blobs: &fakeBlobReader{blobs: map[string][]byte{}}, part: visionFilePart("art-1", data, "image/png", 8, 8), policy: policy, want: ErrorCodeVisionArtifactUnavailable},
		"reader failure":  {blobs: &fakeBlobReader{err: errors.New("boom")}, part: visionFilePart("art-1", data, "image/png", 8, 8), policy: policy, want: ErrorCodeVisionArtifactUnavailable},
		"unwired client":  {blobs: &fakeBlobReader{blobs: map[string][]byte{"art-1": data}}, part: visionFilePart("art-1", data, "image/png", 8, 8), policy: nil, want: ErrorCodeProtocolInvalid},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			client := resolveTestClient(tc.policy, tc.blobs, "openai_chat_compat")
			calls := tc.blobs.calls
			req := visionRequest(tc.part)
			err := client.resolveVisionImages(context.Background(), req)
			if err == nil {
				t.Fatal("expected failure")
			}
			if code, _ := CodeOf(err); code != tc.want {
				t.Fatalf("code = %q, want %q", code, tc.want)
			}
			if tc.want == ErrorCodeVisionBudgetExceeded && tc.blobs.calls != calls {
				t.Fatal("budget rejection must not touch the store")
			}
			if strings.Contains(err.Error(), "art-1") || strings.Contains(err.Error(), visionDigest(data)) {
				t.Fatalf("error discloses reference material: %v", err)
			}
		})
	}
}

func TestResolveVisionImagesBudgetSkipsStore(t *testing.T) {
	cfg := testVisionConfig()
	cfg.MaxImages = 1
	policy := NewVisionPolicy(cfg)
	data := []byte("x")
	blobs := &fakeBlobReader{blobs: map[string][]byte{"a": data, "b": data}}
	client := resolveTestClient(policy, blobs, "openai_chat_compat")
	req := visionRequest(
		visionFilePart("a", data, "image/png", 1, 1),
		visionFilePart("b", data, "image/png", 1, 1),
	)
	if err := client.resolveVisionImages(context.Background(), req); err == nil {
		t.Fatal("expected budget rejection")
	} else if code, _ := CodeOf(err); code != ErrorCodeVisionBudgetExceeded {
		t.Fatalf("code = %q", code)
	}
	if blobs.calls != 0 {
		t.Fatalf("store reads = %d, want 0", blobs.calls)
	}
}

func decodeDataURI(t *testing.T, uri string) (mime string, payload []byte) {
	t.Helper()
	mime, rest, ok := strings.Cut(strings.TrimPrefix(uri, "data:"), ";base64,")
	if !ok {
		t.Fatalf("not a data uri: %.40q", uri)
	}
	raw, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	return mime, raw
}

func TestAdapterImageEquivalenceAcrossProtocols(t *testing.T) {
	data := []byte("equivalent-image-bytes-for-all-protocols")
	req := visionRequest(
		&genai.Part{Text: "before"},
		&genai.Part{InlineData: &genai.Blob{MIMEType: "image/png", Data: data}},
		&genai.Part{Text: "after"},
	)
	bodies := map[string][]byte{}
	var err error
	if bodies["openai_chat_compat"], err = buildOpenAIRequest(req, false, "auto"); err != nil {
		t.Fatalf("openai: %v", err)
	}
	if bodies["openai_responses"], err = buildOpenAIResponsesRequest(req, false); err != nil {
		t.Fatalf("responses: %v", err)
	}
	if bodies["anthropic_messages"], err = buildAnthropicRequest(req, false); err != nil {
		t.Fatalf("anthropic: %v", err)
	}
	geminiBody, geminiErr := buildGeminiRequest(req)
	if geminiErr != nil {
		t.Fatalf("gemini: %v", geminiErr)
	}
	bodies["gemini_native"] = geminiBody

	want := visionDigest(data)
	assertImageBytes := func(protocol string, raw []byte) {
		t.Helper()
		var decoded []byte
		for _, chunk := range strings.Split(string(raw), `"`) {
			if strings.HasPrefix(chunk, "data:image/") {
				_, payload := decodeDataURI(t, chunk)
				decoded = append(decoded, payload...)
			}
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", protocol, err)
		}
		var walk func(v any)
		walk = func(v any) {
			switch value := v.(type) {
			case map[string]any:
				if dataStr, ok := value["data"].(string); ok {
					if _, ok := value["media_type"].(string); ok {
						if payload, err := base64.StdEncoding.DecodeString(dataStr); err == nil {
							decoded = append(decoded, payload...)
						}
					}
					if _, ok := value["mimeType"].(string); ok {
						if payload, err := base64.StdEncoding.DecodeString(dataStr); err == nil {
							decoded = append(decoded, payload...)
						}
					}
				}
				for _, child := range value {
					walk(child)
				}
			case []any:
				for _, child := range value {
					walk(child)
				}
			}
		}
		walk(doc)
		if visionDigest(decoded) != want {
			t.Fatalf("%s: wire bytes do not match the source digest", protocol)
		}
		before := bytes.Index(raw, []byte("before"))
		after := bytes.Index(raw, []byte("after"))
		imageAt := bytes.Index(raw, []byte("image"))
		if before < 0 || after < 0 || imageAt < 0 || before >= imageAt || imageAt >= after {
			t.Fatalf("%s: part order is not preserved", protocol)
		}
	}
	for protocol, body := range bodies {
		assertImageBytes(protocol, body)
	}
}

func TestAdapterRejectsUnresolvedFileReference(t *testing.T) {
	req := visionRequest(visionFilePart("art-1", []byte("x"), "image/png", 1, 1))
	builders := map[string]func() ([]byte, error){
		"openai":    func() ([]byte, error) { return buildOpenAIRequest(req, false, "auto") },
		"responses": func() ([]byte, error) { return buildOpenAIResponsesRequest(req, false) },
		"anthropic": func() ([]byte, error) { return buildAnthropicRequest(req, false) },
		"gemini":    func() ([]byte, error) { return buildGeminiRequest(req) },
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			if _, err := build(); err == nil {
				t.Fatal("expected unresolved reference rejection")
			} else if code, _ := CodeOf(err); code != ErrorCodeProtocolInvalid {
				t.Fatalf("code = %q", code)
			}
		})
	}
}

func visionCapableDefinitions() map[string]config.ModelDefinition {
	return map[string]config.ModelDefinition{
		"plain":  validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{Streaming: true}),
		"sight":  validDefinition(config.ProtocolOpenAIChatCompat, config.ModelCapabilities{Streaming: true, Vision: true}),
		"backup": validDefinition(config.ProtocolAnthropicMessages, config.ModelCapabilities{Streaming: true, Vision: true}),
	}
}

func TestFallbackRejectsVisionlessRoute(t *testing.T) {
	defs := map[string]config.ModelDefinition{"plain": visionCapableDefinitions()["plain"]}
	route := config.ModelRoute{Candidates: []string{"plain"}, MaxProviderAttempts: 1, RetryDelayBudget: config.Duration(time.Second)}
	fallback := NewFallbackAdapter("vision-route", route, defs, nil, MapAdapterResolver{}).WithVisionPolicy(NewVisionPolicy(testVisionConfig()))
	data := []byte("sight-bytes")
	req := visionRequest(visionFilePart("art-1", data, "image/png", 4, 4))
	var gotErr error
	for _, err := range fallback.GenerateContent(context.Background(), req, false) {
		if err != nil {
			gotErr = err
		}
	}
	if gotErr == nil {
		t.Fatal("expected capability rejection")
	}
	if code, _ := CodeOf(gotErr); code != ErrorCodeCapabilityUnsupported {
		t.Fatalf("code = %q, want model_capability_unsupported", code)
	}
}

func TestFallbackRoutesAroundVisionlessCandidate(t *testing.T) {
	defs := visionCapableDefinitions()
	route := config.ModelRoute{Candidates: []string{"plain", "sight"}, MaxProviderAttempts: 2, RetryDelayBudget: config.Duration(time.Second)}
	plainMock := &mockCandidateLLM{name: "plain"}
	sightMock := &mockCandidateLLM{
		name:      "sight",
		responses: []*adkmodel.LLMResponse{{Content: &genai.Content{Parts: []*genai.Part{{Text: "seen"}}}}},
	}
	resolver := MapAdapterResolver{"plain": plainMock, "sight": sightMock}
	fallback := NewFallbackAdapter("vision-route", route, defs, nil, resolver).WithVisionPolicy(NewVisionPolicy(testVisionConfig()))
	data := []byte("sight-bytes")
	req := visionRequest(visionFilePart("art-1", data, "image/png", 4, 4))
	var gotResp *adkmodel.LLMResponse
	for resp, err := range fallback.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
		gotResp = resp
	}
	if gotResp == nil || gotResp.Content.Parts[0].Text != "seen" {
		t.Fatalf("response = %+v", gotResp)
	}
	if len(plainMock.recordedRequests) != 0 {
		t.Fatal("visionless candidate must be skipped before network access")
	}
	if len(sightMock.recordedRequests) != 1 {
		t.Fatal("vision-capable candidate must receive the invocation")
	}
}

func TestFallbackVisionEndToEnd(t *testing.T) {
	data := []byte("end-to-end-image-bytes")
	var captured []byte
	srv := newVisionCaptureServer(t, &captured)
	defer srv.Close()
	blobs := &fakeBlobReader{blobs: map[string][]byte{"art-1": data}}
	policy := NewVisionPolicy(testVisionConfig())
	sight := newOpenAIAdapter(nil, "sight", srv.URL, "", 0, 0, visionWiring{policy: policy, blobs: blobs})
	defs := map[string]config.ModelDefinition{"sight": visionCapableDefinitions()["sight"]}
	route := config.ModelRoute{Candidates: []string{"sight"}, MaxProviderAttempts: 1, RetryDelayBudget: config.Duration(time.Second)}
	fallback := NewFallbackAdapter("vision-route", route, defs, nil, MapAdapterResolver{"sight": sight}).WithVisionPolicy(policy)
	req := visionRequest(&genai.Part{Text: "describe"}, visionFilePart("art-1", data, "image/png", 4, 4))
	for _, err := range fallback.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}
	var doc map[string]any
	if err := json.Unmarshal(captured, &doc); err != nil {
		t.Fatalf("captured request: %v", err)
	}
	serialized, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("remarshal captured request: %v", err)
	}
	if !strings.Contains(string(serialized), "data:image/png;base64,") {
		t.Fatal("wire request carries no inline image")
	}
	if !strings.Contains(string(serialized), `"detail":"auto"`) {
		t.Fatal("wire request carries no detail level")
	}
	_, payload := decodeDataURI(t, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(data))
	if !strings.Contains(string(serialized), base64.StdEncoding.EncodeToString(payload)) {
		t.Fatal("wire image bytes do not match the stored bytes")
	}
	if blobs.calls != 1 {
		t.Fatalf("blob reads = %d, want 1", blobs.calls)
	}
}

func TestMapVisionExhaustion(t *testing.T) {
	unsupported := candidateAttemptError{Candidate: "a", Class: ErrorClassUnsupported, Err: newError(ErrorCodeCapabilityUnsupported, "a", "vision", "no")}
	budget := candidateAttemptError{Candidate: "b", Class: ErrorClassInvalidRequest, Err: newError(ErrorCodeVisionBudgetExceeded, "", "", "too big")}
	transient := candidateAttemptError{Candidate: "c", Class: ErrorClassTransient, Err: newError(ErrorCodeOverloaded, "c", "", "busy")}
	if err := mapVisionExhaustion("route", []candidateAttemptError{unsupported}); err == nil {
		t.Fatal("expected capability error")
	} else if code, _ := CodeOf(err); code != ErrorCodeCapabilityUnsupported {
		t.Fatalf("code = %q", code)
	}
	if err := mapVisionExhaustion("route", []candidateAttemptError{budget}); err == nil {
		t.Fatal("expected budget error")
	} else if code, _ := CodeOf(err); code != ErrorCodeVisionBudgetExceeded {
		t.Fatalf("code = %q", code)
	}
	if err := mapVisionExhaustion("route", []candidateAttemptError{unsupported, transient}); err != nil {
		t.Fatalf("mixed chain must fall through, got %v", err)
	}
	if err := mapVisionExhaustion("route", nil); err != nil {
		t.Fatalf("empty chain must fall through, got %v", err)
	}
}

package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/mail"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/modules/plugin"
	"github.com/anggasct/aura/internal/platform/principal"
	"github.com/anggasct/aura/plugins/gmail/connector"
)

type sentMessage struct {
	threadID string
	header   mail.Header
	body     string
}

func decodeGmailPost(t *testing.T, call *gmailCall) sentMessage {
	t.Helper()
	var payload struct {
		Raw      string `json:"raw"`
		ThreadID string `json:"threadId"`
		Message  *struct {
			Raw      string `json:"raw"`
			ThreadID string `json:"threadId"`
		} `json:"message"`
	}
	if err := json.Unmarshal(call.Body, &payload); err != nil {
		t.Fatalf("decode %s: %v", call.Body, err)
	}
	raw, thread := payload.Raw, payload.ThreadID
	if payload.Message != nil {
		raw, thread = payload.Message.Raw, payload.Message.ThreadID
	}
	decoded, err := base64.URLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(decoded))
	if err != nil {
		t.Fatalf("parse message: %v\n%s", err, decoded)
	}
	var body bytes.Buffer
	if _, err := body.ReadFrom(parsed.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	text, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body.String(), "\r\n", ""))
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return sentMessage{threadID: thread, header: parsed.Header, body: string(text)}
}

func TestGmailManifestAgreesWithTheConnector(t *testing.T) {
	data, err := os.ReadFile(gmailManifestSource)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	manifest, err := plugin.ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if manifest.Backend.Kind != plugin.BackendConnector || manifest.Auth.Kind != plugin.AuthOAuth2 {
		t.Fatalf("backend = %q auth = %q", manifest.Backend.Kind, manifest.Auth.Kind)
	}
	if size := manifest.ServerConfig("/bundle").MaxMessageSize; size < 640<<10 {
		t.Fatalf("max_message_size = %d, want at least 640 KiB", size)
	}
	described := map[string]bool{}
	for _, tool := range connector.New().Tools() {
		described[tool.Name] = true
	}
	var declared []string
	for i := range manifest.Tools {
		tool := &manifest.Tools[i]
		declared = append(declared, tool.Name)
		if !described[tool.Name] {
			t.Fatalf("manifest declares %s, which the connector does not describe", tool.Name)
		}
		wantEffect := tool.Name == "draft" || tool.Name == "send"
		if (tool.Tier == plugin.TierEffect) != wantEffect || (wantEffect && !slices.Equal(tool.Display, []string{"to", "cc", "subject", "reply_to_id", "body"})) {
			t.Fatalf("tool %s tier %s display %v", tool.Name, tool.Tier, tool.Display)
		}
	}
	slices.Sort(declared)
	if len(described) != len(declared) || !slices.Equal(declared, []string{"draft", "read", "search", "send"}) {
		t.Fatalf("declared = %v, described = %d", declared, len(described))
	}
}

func TestGmailBundleInstallsReviewsAndApprovesShowingScopesHostsAndTools(t *testing.T) {
	cfg := writeChildrenCLIConfig(t)
	bundle := buildGmailBundle(t)
	out, err := runPluginCommand(t, cfg, "install", bundle)
	if err != nil || !strings.Contains(out, "installed gmail 0.1.0 as quarantined") {
		t.Fatalf("install = %q, %v", out, err)
	}
	review, err := runPluginCommand(t, cfg, "review", "gmail")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	for _, want := range []string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/gmail.compose",
		"Egress: gmail.googleapis.com, oauth2.googleapis.com",
		"issuer https://accounts.google.com",
		"authorize params access_type=offline prompt=consent",
		"gmail.search [read]",
		"gmail.read [read]",
		"gmail.draft [effect]",
		"gmail.send [effect]",
		"approval shows, in order: to, cc, subject, reply_to_id, body",
		"client_id -> env://AURA_GMAIL_CLIENT_ID",
		"Backend: connector ./bin/aura-connector-gmail",
	} {
		if !strings.Contains(review, want) {
			t.Errorf("review does not show %q:\n%s", want, review)
		}
	}
	approved, err := runPluginCommand(t, cfg, "approve", "gmail", "--digest", pluginDigestFromReview(t, review)[:16])
	if err != nil || !strings.Contains(approved, "approved gmail 0.1.0") || !strings.Contains(approved, "4 tools") {
		t.Fatalf("approve = %q, %v", approved, err)
	}
	listed, err := runPluginCommand(t, cfg, "list")
	if err != nil || !strings.Contains(listed, "active") {
		t.Fatalf("list = %q, %v", listed, err)
	}
}

func TestGmailSearchAndReadUseGetOnlyAndReturnBoundedResults(t *testing.T) {
	f := newGmailFixture(t, nil)
	var ids []string
	for i := range 15 {
		id := fmt.Sprintf("m%d", i)
		ids = append(ids, id)
		f.api.setMessage(t, id, gmailMessage(id, gmailHeaders("From", "Alice <alice@example.com>", "Subject", "Subject "+id, "Date", "Tue, 3 Nov 2026 10:00:00 +0000")))
	}
	f.api.setList(ids...)
	output, err := f.owner(t, "search", "search-1", map[string]any{"query": "from:alice", "max_results": 15})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var found struct {
		Messages []map[string]string `json:"messages"`
	}
	if err := json.Unmarshal(output, &found); err != nil || len(found.Messages) != 15 || found.Messages[3]["subject"] != "Subject m3" || found.Messages[3]["thread_id"] != "thread-m3" {
		t.Fatalf("output = %s (%v)", output, err)
	}
	calls := f.api.recorded()
	if len(calls) != 16 {
		t.Fatalf("api calls = %d, want one list and fifteen gets", len(calls))
	}
	for _, call := range calls {
		if call.Method != http.MethodGet || call.Authorization != "Bearer "+gmailBearer {
			t.Fatalf("call = %+v", call)
		}
	}

	body := strings.Repeat("日", 20000)
	f.api.setMessage(t, "big", gmailMessage("big", gmailHeaders("Subject", "Long"), gmailPart("text/plain", body)))
	output, err = f.owner(t, "read", "read-1", map[string]any{"id": "big"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var read struct {
		Body      string `json:"body"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(output, &read); err != nil || read.Truncated || read.Body != body {
		t.Fatalf("read = %d bytes truncated=%v (%v): %.300s", len(read.Body), read.Truncated, err, output)
	}
	if len(f.h.promptedTools()) != 0 {
		t.Fatalf("read tools asked for approval: %v", f.h.promptedTools())
	}
	if len(f.api.mutations()) != 0 {
		t.Fatalf("a read tool changed state: %+v", f.api.mutations())
	}
}

func TestGmailReadReducesHiddenHTMLAndKeepsPlantedTextOutOfErrors(t *testing.T) {
	f := newGmailFixture(t, nil)
	markup := `<html><head><style>p{}</style></head><body><p>Quarterly update</p><div style="display:none">SECRET-HIDDEN</div><script>SECRET-SCRIPT()</script><!-- SECRET-COMMENT --></body></html>`
	f.api.setMessage(t, "html", gmailMessage("html", gmailHeaders("Subject", "Update"), gmailPart("text/html", markup)))
	output, err := f.owner(t, "read", "read-html", map[string]any{"id": "html"})
	if err != nil || !strings.Contains(string(output), "Quarterly update") || strings.Contains(string(output), "SECRET") {
		t.Fatalf("read = %s, %v", output, err)
	}
	_, err = f.owner(t, "read", "read-missing", map[string]any{"id": "absent"})
	if !connectorCode(err, "plugin_connector_error: not_found") || strings.Contains(err.Error(), "PLANTED") {
		t.Fatalf("read of a missing mail = %v", err)
	}
}

func TestGmailOversizedMailReturnsAnErrorAndKeepsTheConnector(t *testing.T) {
	f := newGmailFixture(t, nil)
	f.api.setRawMessage("huge", []byte(`{"id":"huge","threadId":"t","payload":{"mimeType":"text/plain","body":{"data":"`+strings.Repeat("A", 2<<20)+`"}}}`))
	f.api.setMessage(t, "small", gmailMessage("small", gmailHeaders("Subject", "Small"), gmailPart("text/plain", "fits")))
	_, err := f.owner(t, "read", "huge-1", map[string]any{"id": "huge"})
	if !connectorCode(err, "plugin_connector_error: message_too_large") {
		t.Fatalf("read of an oversized mail = %v", err)
	}
	output, err := f.owner(t, "read", "small-1", map[string]any{"id": "small"})
	if err != nil || !strings.Contains(string(output), "fits") {
		t.Fatalf("read after the oversized mail = %s, %v", output, err)
	}
	f.process.mu.Lock()
	defer f.process.mu.Unlock()
	if len(f.process.requests) != 1 {
		t.Fatalf("sessions started = %d, an oversized mail must not restart the connector", len(f.process.requests))
	}
}

func TestGmailSendNeedsApprovalShowingEveryRecipientFirstAndIsJournaled(t *testing.T) {
	f := newGmailFixture(t, nil)
	recipients := make([]string, 0, 12)
	for i := range 12 {
		recipients = append(recipients, fmt.Sprintf("recipient%d@example.com", i))
	}
	body := strings.Repeat("long body line\n", 400)
	arguments := map[string]any{
		"body":    body,
		"subject": "Café planning ☕",
		"cc":      []string{"copy@example.org"},
		"to":      recipients,
	}
	output, err := f.owner(t, "send", "send-1", arguments)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !strings.Contains(string(output), `"status":"sent"`) {
		t.Fatalf("output = %s", output)
	}
	if prompts := f.h.promptedTools(); !slices.Equal(prompts, []string{"plugin_gmail_send"}) {
		t.Fatalf("approval prompts = %v", prompts)
	}
	shown := f.h.promptedArguments()[0]
	last := -1
	for _, field := range []string{"to:", "cc:", "subject:", "body:"} {
		index := strings.Index(shown, field)
		if index <= last {
			t.Fatalf("%q at %d does not follow the earlier fields in the approval prompt:\n%s", field, index, shown)
		}
		last = index
	}
	for _, recipient := range append(recipients, "copy@example.org") {
		if !strings.Contains(shown, recipient) {
			t.Fatalf("approval prompt hides %s:\n%s", recipient, shown)
		}
	}
	if countEffectsIn(t, f.h.db, "succeeded") != 1 {
		t.Fatal("the send was not journaled as succeeded")
	}
	posts := f.api.mutations()
	if len(posts) != 1 || posts[0].Path != gmailAPIPrefix+"messages/send" || posts[0].Authorization != "Bearer "+gmailBearer {
		t.Fatalf("mutations = %+v", posts)
	}
	sent := decodeGmailPost(t, &posts[0])
	subject, err := new(mime.WordDecoder).DecodeHeader(sent.header.Get("Subject"))
	if err != nil || subject != "Café planning ☕" {
		t.Fatalf("subject = %q, %v", subject, err)
	}
	to, err := sent.header.AddressList("To")
	if err != nil || len(to) != 12 {
		t.Fatalf("To = %v, %v", to, err)
	}
	if sent.header.Get("Cc") != "copy@example.org" || sent.header.Get("From") != "" || sent.header.Get("Bcc") != "" || sent.body != strings.ReplaceAll(body, "\n", "\r\n") {
		t.Fatalf("header = %v, body %d bytes", sent.header, len(sent.body))
	}
}

func TestGmailDeclinedApprovalSendsNothing(t *testing.T) {
	f := newGmailFixture(t, nil)
	f.h.mu.Lock()
	f.h.approve = false
	f.h.mu.Unlock()
	_, err := f.owner(t, "draft", "draft-declined", map[string]any{"to": []string{"a@example.com"}, "subject": "Hi", "body": "x"})
	if err == nil {
		t.Fatal("a declined draft succeeded")
	}
	if len(f.api.recorded()) != 0 || len(f.h.promptedTools()) != 1 {
		t.Fatalf("api calls = %+v, prompts = %v", f.api.recorded(), f.h.promptedTools())
	}
}

func TestGmailDraftAndReplyBuildThreadedMIME(t *testing.T) {
	f := newGmailFixture(t, nil)
	f.api.setMessage(t, "orig", gmailMessage("orig", gmailHeaders("Subject", "Budget review", "Message-ID", "<orig-1@mail.example.com>", "References", "<root-1@mail.example.com>")))
	output, err := f.owner(t, "draft", "draft-1", map[string]any{
		"to": []string{"boss@example.com"}, "subject": "Re: Budget review", "body": "Looks good.", "reply_to_id": "orig",
	})
	if err != nil || !strings.Contains(string(output), `"status":"drafted"`) || !strings.Contains(string(output), `"draft_id":"r-1"`) {
		t.Fatalf("draft = %s, %v", output, err)
	}
	calls := f.api.recorded()
	if len(calls) != 2 || calls[0].Method != http.MethodGet || calls[1].Method != http.MethodPost || calls[1].Path != gmailAPIPrefix+"drafts" {
		t.Fatalf("calls = %+v", calls)
	}
	sent := decodeGmailPost(t, &calls[1])
	if sent.threadID != "thread-orig" || sent.header.Get("In-Reply-To") != "<orig-1@mail.example.com>" ||
		strings.Join(strings.Fields(sent.header.Get("References")), " ") != "<root-1@mail.example.com> <orig-1@mail.example.com>" {
		t.Fatalf("thread = %q header = %v", sent.threadID, sent.header)
	}
	if prompts := f.h.promptedTools(); !slices.Equal(prompts, []string{"plugin_gmail_draft"}) {
		t.Fatalf("approval prompts = %v", prompts)
	}
}

func TestGmailRejectedInputNeverReachesGmail(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"to": []string{"a@example.com"}, "subject": "Hi", "body": "x"}
	}
	with := func(key string, value any) map[string]any {
		arguments := base()
		arguments[key] = value
		return arguments
	}
	cases := []struct {
		name      string
		tool      string
		arguments map[string]any
	}{
		{"header injection in to", "send", with("to", []string{"a@example.com\r\nBcc: eve@example.com"})},
		{"header injection in cc", "send", with("cc", []string{"a@example.com\r\nBcc: eve@example.com"})},
		{"header injection in the subject", "send", with("subject", "Hi\r\nBcc: eve@example.com")},
		{"display name", "send", with("to", []string{"Alice <a@example.com>"})},
		{"group address", "draft", with("to", []string{"Team: a@example.com;"})},
		{"non-ascii address", "send", with("to", []string{"älice@example.com"})},
		{"too many recipients", "send", with("to", slices.Repeat([]string{"a@example.com"}, 21))},
		{"path traversal in the reply id", "send", with("reply_to_id", "../../drafts")},
		{"extra field", "send", with("bcc", []string{"eve@example.com"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGmailFixture(t, nil)
			if _, err := f.owner(t, tc.tool, "bad-"+tc.tool, tc.arguments); err == nil {
				t.Fatal("a bad input was accepted")
			}
			if calls := f.api.recorded(); len(calls) != 0 {
				t.Fatalf("a rejected input reached gmail: %+v", calls)
			}
			if countEffectsIn(t, f.h.db, "succeeded") != 0 || countEffectsIn(t, f.h.db, "unknown") != 0 {
				t.Fatal("a rejected input was journaled as sent")
			}
		})
	}
}

func TestGmailBadReadIDsNeverReachGmail(t *testing.T) {
	f := newGmailFixture(t, nil)
	for _, id := range []string{"../drafts", "a/b", "m1?format=raw", "", strings.Repeat("a", 65)} {
		if _, err := f.owner(t, "read", "read-bad", map[string]any{"id": id}); err == nil {
			t.Fatalf("read accepted id %q", id)
		}
	}
	if calls := f.api.recorded(); len(calls) != 0 {
		t.Fatalf("a bad id reached gmail: %+v", calls)
	}
}

func TestGmailReplyWithAChangedSubjectFailsWithoutAMutatingRequest(t *testing.T) {
	f := newGmailFixture(t, nil)
	f.api.setMessage(t, "orig", gmailMessage("orig", gmailHeaders("Subject", "Budget review", "Message-ID", "<orig-1@mail.example.com>")))
	_, err := f.owner(t, "send", "send-changed", map[string]any{
		"to": []string{"boss@example.com"}, "subject": "Wire the money now", "body": "x", "reply_to_id": "orig",
	})
	if !connectorCode(err, "plugin_connector_error: subject_mismatch") {
		t.Fatalf("send with a changed subject = %v", err)
	}
	if len(f.api.mutations()) != 0 {
		t.Fatalf("a mutating request was made: %+v", f.api.mutations())
	}
	if countEffectsIn(t, f.h.db, "failed") != 1 || countEffectsIn(t, f.h.db, "unknown") != 0 {
		t.Fatalf("failed=%d unknown=%d", countEffectsIn(t, f.h.db, "failed"), countEffectsIn(t, f.h.db, "unknown"))
	}
}

func TestGmailRefusalSettlesFailedAndNeverShowsProviderText(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newGmailFixture(t, nil)
			f.api.setPost(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"` + gmailPlanted + `"}}`))
			})
			_, err := f.owner(t, "send", "send-refused", map[string]any{"to": []string{"a@example.com"}, "subject": "Hi", "body": "x"})
			if err == nil || !strings.Contains(err.Error(), "plugin_connector_error") || strings.Contains(err.Error(), "PLANTED") {
				t.Fatalf("send = %v", err)
			}
			if countEffectsIn(t, f.h.db, "failed") != 1 || countEffectsIn(t, f.h.db, "unknown") != 0 || countEffectsIn(t, f.h.db, "succeeded") != 0 {
				t.Fatalf("failed=%d unknown=%d succeeded=%d", countEffectsIn(t, f.h.db, "failed"), countEffectsIn(t, f.h.db, "unknown"), countEffectsIn(t, f.h.db, "succeeded"))
			}
			if gmailPostCount(f.api.recorded()) != 1 {
				t.Fatalf("posts = %d, a refusal must not be retried", gmailPostCount(f.api.recorded()))
			}
		})
	}
}

func TestGmailAmbiguousSendIsNeverResent(t *testing.T) {
	cases := []struct {
		name string
		post func(w http.ResponseWriter, r *http.Request)
	}{
		{"server error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"` + gmailPlanted + `"}}`))
		}},
		{"dropped connection", func(w http.ResponseWriter, _ *http.Request) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			if conn, _, err := hijacker.Hijack(); err == nil {
				_ = conn.Close()
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGmailFixture(t, nil)
			f.api.setPost(tc.post)
			_, err := f.owner(t, "send", "send-ambiguous", map[string]any{"to": []string{"a@example.com"}, "subject": "Hi", "body": "x"})
			if !connectorCode(err, "plugin_request_failed") || strings.Contains(err.Error(), "PLANTED") {
				t.Fatalf("send = %v, want plugin_request_failed", err)
			}
			if countEffectsIn(t, f.h.db, "unknown") != 1 || countEffectsIn(t, f.h.db, "succeeded") != 0 {
				t.Fatalf("unknown=%d succeeded=%d", countEffectsIn(t, f.h.db, "unknown"), countEffectsIn(t, f.h.db, "succeeded"))
			}
			if gmailPostCount(f.api.recorded()) != 1 {
				t.Fatalf("posts = %d, an ambiguous send must not be repeated", gmailPostCount(f.api.recorded()))
			}
		})
	}
}

func TestGmailToolsAreDeniedToPublicPrincipals(t *testing.T) {
	f := newGmailFixture(t, nil)
	for _, tool := range []string{"search", "read", "draft", "send"} {
		_, err := f.call(t, tool, "public-"+tool, principal.ClassPublic, map[string]any{"id": "m1", "query": "x"})
		if err == nil {
			t.Fatalf("public %s was allowed", tool)
		}
	}
	if len(f.api.recorded()) != 0 {
		t.Fatalf("a refused call reached gmail: %+v", f.api.recorded())
	}
}

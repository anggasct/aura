//go:build linux && integration

package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/sandbox"
)

func TestIntegrationGmailConnectorRunsInsideTheSandbox(t *testing.T) {
	f := newGmailFixture(t, newConnectorSessionStarter(sandbox.Start))
	f.api.setList("m1", "m2")
	for _, id := range []string{"m1", "m2"} {
		f.api.setMessage(t, id, gmailMessage(id, gmailHeaders("From", "Alice <alice@example.com>", "Subject", "Café "+id, "Date", "Tue, 3 Nov 2026 10:00:00 +0000"),
			gmailPart("text/html", `<p>Body of `+id+`</p><div style="display:none">SECRET</div>`)))
	}

	output, err := f.owner(t, "search", "sandbox-search", map[string]any{"query": "from:alice", "max_results": 2})
	if err != nil {
		t.Fatalf("search inside the sandbox: %v", err)
	}
	var found struct {
		Messages []map[string]string `json:"messages"`
	}
	if err := json.Unmarshal(output, &found); err != nil || len(found.Messages) != 2 || found.Messages[0]["subject"] != "Café m1" {
		t.Fatalf("search output = %s (%v)", output, err)
	}

	output, err = f.owner(t, "read", "sandbox-read", map[string]any{"id": "m2"})
	if err != nil || !strings.Contains(string(output), "Body of m2") || strings.Contains(string(output), "SECRET") {
		t.Fatalf("read output = %s (%v)", output, err)
	}

	output, err = f.owner(t, "send", "sandbox-send", map[string]any{"to": []string{"boss@example.com"}, "subject": "Rapat ☕", "body": "Selamat pagi"})
	if err != nil || !strings.Contains(string(output), `"status":"sent"`) {
		t.Fatalf("send output = %s (%v)", output, err)
	}
	posts := f.api.mutations()
	if len(posts) != 1 {
		t.Fatalf("mutations = %+v", posts)
	}
	if sent := decodeGmailPost(t, &posts[0]); sent.body != "Selamat pagi" || sent.header.Get("To") != "boss@example.com" {
		t.Fatalf("sent message = %+v", sent)
	}
	if countEffectsIn(t, f.h.db, "succeeded") != 1 {
		t.Fatal("the send was not journaled as succeeded")
	}
	for _, call := range f.api.recorded() {
		if call.Authorization != "Bearer "+gmailBearer {
			t.Fatalf("call without the host-injected credential: %+v", call)
		}
	}
}

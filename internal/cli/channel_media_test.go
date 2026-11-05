package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/channel/media"
	"github.com/anggasct/aura/internal/store"
)

func TestChannelMediaStorePersistsContentUnderTheSession(t *testing.T) {
	db := discordTestDB(t)
	sessionID := "main/telegram:pocket/chat:5"
	if err := (&discordSessionEnsurer{sessions: store.NewSessionService(db)}).EnsureSession(t.Context(), sessionID, "telegram:5", "main"); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	artifacts := store.NewArtifactStore(db, t.TempDir(), 1<<20)
	channelStore := &channelMediaStore{store: artifacts}

	receipt, err := channelStore.Put(t.Context(), strings.NewReader("pdf-bytes"), &media.Meta{SessionID: sessionID, Filename: "a.pdf", MediaType: "application/pdf", Extra: []byte(`{"message_id":"5:9"}`)})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	sum := sha256.Sum256([]byte("pdf-bytes"))
	if receipt.Digest != hex.EncodeToString(sum[:]) || receipt.SizeBytes != 9 || !strings.HasPrefix(receipt.RefID, "art-") {
		t.Errorf("receipt = %+v", receipt)
	}
	reader, meta, err := artifacts.Open(t.Context(), receipt.RefID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != "pdf-bytes" || meta.Filename != "a.pdf" || meta.SessionID != sessionID {
		t.Errorf("stored = %q %+v (%v)", body, meta, err)
	}
}

package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/storage"
	"github.com/anggasct/aura/internal/platform/turn"
	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
	"github.com/anggasct/aura/internal/store/storetest"
)

func TestChannelMediaStorePersistsContentUnderTheSession(t *testing.T) {
	db := storetest.OpenMigrated(t)
	sessionID := "main/telegram:pocket/chat:5"
	now := time.Now().UTC()
	if err := turnstore.NewSessionService(db).Create(t.Context(), &turn.Session{
		ID:        sessionID,
		OwnerID:   "telegram:5",
		AgentID:   "main",
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("Create session: %v", err)
	}
	artifacts := storage.NewArtifactStore(db, t.TempDir(), 1<<20)
	channelStore, err := NewArtifactStore(artifacts)
	if err != nil {
		t.Fatalf("NewArtifactStore: %v", err)
	}

	receipt, err := channelStore.Put(t.Context(), strings.NewReader("pdf-bytes"), &Meta{SessionID: sessionID, Filename: "a.pdf", MediaType: "application/pdf", Extra: []byte(`{"message_id":"5:9"}`)})
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

func TestArtifactStoreRejectsNilArguments(t *testing.T) {
	if _, err := NewArtifactStore(nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("NewArtifactStore(nil) = %v, want invalid_argument", err)
	}
	store, err := NewArtifactStore(storage.NewArtifactStore(storetest.OpenMigrated(t), t.TempDir(), 1<<20))
	if err != nil {
		t.Fatalf("NewArtifactStore: %v", err)
	}
	if _, err := store.Put(t.Context(), strings.NewReader("x"), nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("Put(nil meta) = %v, want invalid_argument", err)
	}
}

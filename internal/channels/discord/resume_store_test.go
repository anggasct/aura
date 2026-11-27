package discord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/platform/fault"
	"github.com/anggasct/aura/internal/platform/sqlite"
	"github.com/anggasct/aura/internal/platform/sqlite/sqlitetest"
)

func mustResumeStore(t *testing.T, instance string) (store *SQLiteResumeStore, corrupt func() error) {
	t.Helper()
	db := sqlitetest.OpenMigrated(t, Schema())
	s, err := NewSQLiteResumeStore(db, instance)
	if err != nil {
		t.Fatalf("NewSQLiteResumeStore(): %v", err)
	}
	return s, func() error {
		_, err := db.ExecContext(t.Context(),
			`UPDATE channel_resume SET updated_at = 'not-a-timestamp' WHERE source = 'discord' AND instance = ?`, instance)
		return err
	}
}

func TestChannelResumeStore_SaveLoadDelete(t *testing.T) {
	s, _ := mustResumeStore(t, "owner")
	ctx := t.Context()

	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	cursor := ResumeCursor{
		SessionID:    "session-one",
		Sequence:     42,
		ConfigDigest: "digest-one",
		UpdatedAt:    now,
	}
	if err := s.Save(ctx, &cursor); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	got, found, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !found {
		t.Fatalf("Load() found = false, want true")
	}
	if got.SessionID != "session-one" || got.Sequence != 42 || got.ConfigDigest != "digest-one" {
		t.Errorf("loaded resume mismatch: %+v", got)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Errorf("updated_at = %v, want %v", got.UpdatedAt, now)
	}

	cursor.Sequence = 43
	cursor.ConfigDigest = "digest-two"
	if err := s.Save(ctx, &cursor); err != nil {
		t.Fatalf("Save() update: %v", err)
	}
	got, found, err = s.Load(ctx)
	if err != nil || !found {
		t.Fatalf("Load() after update: %v, found = %v", err, found)
	}
	if got.Sequence != 43 || got.ConfigDigest != "digest-two" {
		t.Errorf("updated resume mismatch: %+v", got)
	}

	if err := s.Delete(ctx); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	_, found, err = s.Load(ctx)
	if err != nil {
		t.Fatalf("Load() after delete: %v", err)
	}
	if found {
		t.Errorf("Load() found = true after delete, want false")
	}
}

func TestChannelResumeStore_SchemaPresent(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schema())
	var name string
	err := db.QueryRowContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'channel_resume'`).Scan(&name)
	if err != nil {
		t.Fatalf("channel_resume table missing after migrate: %v", err)
	}
}

func TestChannelResumeStore_RejectsInvalid(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schema())
	if _, err := NewSQLiteResumeStore(nil, "owner"); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("NewSQLiteResumeStore(nil) = %v, want invalid_argument", err)
	}
	if _, err := NewSQLiteResumeStore(db, ""); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("NewSQLiteResumeStore(empty instance) = %v, want invalid_argument", err)
	}
	s, err := NewSQLiteResumeStore(db, "owner")
	if err != nil {
		t.Fatalf("NewSQLiteResumeStore(): %v", err)
	}
	ctx := t.Context()

	if err := s.Save(ctx, nil); !fault.HasCode(err, ErrorCodeInvalidArgument) {
		t.Errorf("Save(nil) = %v, want invalid_argument", err)
	}

	base := ResumeCursor{
		SessionID:    "session-one",
		Sequence:     1,
		ConfigDigest: "digest",
		UpdatedAt:    time.Now().UTC(),
	}
	cases := map[string]func(*ResumeCursor){
		"empty session": func(r *ResumeCursor) { r.SessionID = "" },
		"negative seq":  func(r *ResumeCursor) { r.Sequence = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := base
			mutate(&candidate)
			err := s.Save(ctx, &candidate)
			if !fault.HasCode(err, ErrorCodeInvalidArgument) {
				t.Errorf("Save() error = %v, want invalid_argument", err)
			}
		})
	}
}

func TestChannelResumeStore_RejectsCorruptTimestamp(t *testing.T) {
	s, corrupt := mustResumeStore(t, "owner")
	ctx := t.Context()

	cursor := ResumeCursor{
		SessionID:    "session-one",
		Sequence:     1,
		ConfigDigest: "digest",
		UpdatedAt:    time.Now().UTC(),
	}
	if err := s.Save(ctx, &cursor); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	if err := corrupt(); err != nil {
		t.Fatalf("corrupt fixture: %v", err)
	}
	_, _, err := s.Load(ctx)
	if !fault.HasCode(err, ErrorCodeResumeInvalid) {
		t.Errorf("Load() error = %v, want channel_resume_invalid", err)
	}
}

func TestChannelResumeStore_ParticipatesInTransaction(t *testing.T) {
	db := sqlitetest.OpenMigrated(t, Schema())
	s, err := NewSQLiteResumeStore(db, "owner")
	if err != nil {
		t.Fatalf("NewSQLiteResumeStore(): %v", err)
	}
	ctx := t.Context()
	rollbackErr := errors.New("rollback sentinel")

	err = sqlite.RunInTx(ctx, db, func(txCtx context.Context) error {
		if saveErr := s.Save(txCtx, &ResumeCursor{
			SessionID:    "tx-session",
			Sequence:     9,
			ConfigDigest: "tx-digest",
			UpdatedAt:    time.Now().UTC(),
		}); saveErr != nil {
			return saveErr
		}
		got, found, loadErr := s.Load(txCtx)
		if loadErr != nil {
			return loadErr
		}
		if !found || got.SessionID != "tx-session" {
			t.Errorf("Load inside tx = %+v, found=%v", got, found)
		}
		return rollbackErr
	})
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("RunInTx() = %v, want rollback sentinel", err)
	}

	_, found, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load after rollback: %v", err)
	}
	if found {
		t.Fatal("row saved inside rolled-back transaction remained visible")
	}
}

package store

import (
	"testing"
	"time"
)

func TestChannelResumeStore_SaveLoadDelete(t *testing.T) {
	db := newTestDB(t)
	s := NewChannelResumeStore(db)
	ctx := t.Context()

	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	resume := ChannelResume{
		Source:           "discord",
		Instance:         "owner",
		GatewaySessionID: "session-one",
		LastSequence:     42,
		ConfigDigest:     "digest-one",
		UpdatedAt:        now,
	}
	if err := s.Save(ctx, &resume); err != nil {
		t.Fatalf("Save(): %v", err)
	}

	got, found, err := s.Load(ctx, "discord", "owner")
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !found {
		t.Fatalf("Load() found = false, want true")
	}
	if got.GatewaySessionID != "session-one" || got.LastSequence != 42 || got.ConfigDigest != "digest-one" {
		t.Errorf("loaded resume mismatch: %+v", got)
	}
	if !got.UpdatedAt.Equal(now) {
		t.Errorf("updated_at = %v, want %v", got.UpdatedAt, now)
	}

	resume.LastSequence = 43
	resume.ConfigDigest = "digest-two"
	if err := s.Save(ctx, &resume); err != nil {
		t.Fatalf("Save() update: %v", err)
	}
	got, found, err = s.Load(ctx, "discord", "owner")
	if err != nil || !found {
		t.Fatalf("Load() after update: %v, found = %v", err, found)
	}
	if got.LastSequence != 43 || got.ConfigDigest != "digest-two" {
		t.Errorf("updated resume mismatch: %+v", got)
	}

	if err := s.Delete(ctx, "discord", "owner"); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	_, found, err = s.Load(ctx, "discord", "owner")
	if err != nil {
		t.Fatalf("Load() after delete: %v", err)
	}
	if found {
		t.Errorf("Load() found = true after delete, want false")
	}
}

func TestChannelResumeStore_SchemaPresent(t *testing.T) {
	db := newTestDB(t)
	var name string
	err := db.QueryRowContext(t.Context(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'channel_resume'`).Scan(&name)
	if err != nil {
		t.Fatalf("channel_resume table missing after migrate: %v", err)
	}
}

func TestChannelResumeStore_RejectsInvalid(t *testing.T) {
	db := newTestDB(t)
	s := NewChannelResumeStore(db)
	ctx := t.Context()

	base := ChannelResume{
		Source:           "discord",
		Instance:         "owner",
		GatewaySessionID: "session-one",
		LastSequence:     1,
		ConfigDigest:     "digest",
		UpdatedAt:        time.Now().UTC(),
	}
	cases := map[string]func(*ChannelResume){
		"empty source":   func(r *ChannelResume) { r.Source = "" },
		"empty instance": func(r *ChannelResume) { r.Instance = "" },
		"empty session":  func(r *ChannelResume) { r.GatewaySessionID = "" },
		"negative seq":   func(r *ChannelResume) { r.LastSequence = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := base
			mutate(&candidate)
			err := s.Save(ctx, &candidate)
			code, ok := CodeOf(err)
			if !ok || code != ErrorCodeInvalidArgument {
				t.Errorf("Save() code = %v, %v; want invalid_argument", code, ok)
			}
		})
	}
}

func TestChannelResumeStore_RejectsCorruptTimestamp(t *testing.T) {
	db := newTestDB(t)
	s := NewChannelResumeStore(db)
	ctx := t.Context()

	resume := ChannelResume{
		Source:           "discord",
		Instance:         "owner",
		GatewaySessionID: "session-one",
		LastSequence:     1,
		ConfigDigest:     "digest",
		UpdatedAt:        time.Now().UTC(),
	}
	if err := s.Save(ctx, &resume); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE channel_resume SET updated_at = 'not-a-timestamp' WHERE source = 'discord' AND instance = 'owner'`); err != nil {
		t.Fatalf("corrupt fixture: %v", err)
	}
	_, _, err := s.Load(ctx, "discord", "owner")
	code, ok := CodeOf(err)
	if !ok || code != ErrorCodeChannelResumeInvalid {
		t.Errorf("Load() code = %v, %v; want channel_resume_invalid", code, ok)
	}
}

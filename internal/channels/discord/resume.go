package discord

import (
	"context"
	"time"
)

type ResumeCursor struct {
	SessionID    string
	Sequence     int64
	ConfigDigest string
	UpdatedAt    time.Time
}

type ResumeStore interface {
	Save(ctx context.Context, cursor *ResumeCursor) error
	Load(ctx context.Context) (ResumeCursor, bool, error)
	Delete(ctx context.Context) error
}

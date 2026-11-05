package email

import (
	"context"
	"time"
)

type MailboxStatus struct {
	UIDValidity uint32
	UIDNext     uint32
}

type MessageInfo struct {
	UID  uint32
	Size int64
}

type Session interface {
	Select(ctx context.Context, folder string) (MailboxStatus, error)
	List(ctx context.Context, first, last uint32) ([]MessageInfo, error)
	Header(ctx context.Context, uid uint32, limit int64) ([]byte, error)
	Message(ctx context.Context, uid uint32, limit int64) ([]byte, error)
	Wait(ctx context.Context, interval time.Duration) error
	Close() error
}

type Connector interface {
	Connect(ctx context.Context) (Session, error)
}

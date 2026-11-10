package email

import (
	"context"
	"slices"
	"time"
)

func (a *Adapter) run(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		cycles, err := a.serve(ctx)
		if ctx.Err() != nil {
			return
		}
		if cycles > 0 {
			attempt = 0
		}
		a.markFault(classifyFault(err))
		a.logger.WarnContext(ctx, "email session failed", "component", "email", "account", a.account.Name, "fault", a.currentFault())
		if !sleepContext(ctx, a.retryDelay(attempt)) {
			return
		}
		attempt++
	}
}

func (a *Adapter) serve(ctx context.Context) (cycles int, err error) {
	session, err := a.connector.Connect(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = session.Close() }()
	interval := time.Duration(a.account.IMAP.PollInterval)
	for {
		if err := a.cycle(ctx, session); err != nil {
			return cycles, err
		}
		cycles++
		a.markLive()
		if err := session.Wait(ctx, interval); err != nil {
			return cycles, err
		}
	}
}

func (a *Adapter) cycle(ctx context.Context, session Session) error {
	status, err := session.Select(ctx, a.account.IMAP.Folder)
	if err != nil {
		return err
	}
	if status.UIDValidity == 0 || status.UIDNext == 0 {
		return Errorf(ErrorCodeProtocolInvalid, "mailbox did not report its identity")
	}
	cursor, err := a.syncCursor(ctx, &status)
	if err != nil {
		return err
	}
	for cursor.LastUID < status.UIDNext-1 {
		first := cursor.LastUID + 1
		last := status.UIDNext - 1
		if last-first >= a.window {
			last = first + a.window - 1
		}
		next, err := a.ingestWindow(ctx, session, &cursor, first, last)
		if err != nil {
			return err
		}
		cursor = next
	}
	return nil
}

func (a *Adapter) syncCursor(ctx context.Context, status *MailboxStatus) (Cursor, error) {
	stored, found, err := a.cursors.Load(ctx, a.source())
	if err != nil {
		return Cursor{}, &Error{Code: ErrorCodeIntakeFailed, Detail: "email cursor is not readable", Cause: err}
	}
	if found && stored.UIDValidity == status.UIDValidity {
		return stored, nil
	}
	baseline := Cursor{UIDValidity: status.UIDValidity, LastUID: status.UIDNext - 1}
	if err := a.saveCursor(ctx, baseline); err != nil {
		return Cursor{}, err
	}
	if found {
		a.validityChanged.Store(true)
		a.logger.WarnContext(ctx, "email mailbox identity changed", "component", "email", "account", a.account.Name)
	}
	return baseline, nil
}

func (a *Adapter) saveCursor(ctx context.Context, cursor Cursor) error {
	if err := a.cursors.Save(ctx, a.source(), cursor, time.Now().UTC()); err != nil {
		return &Error{Code: ErrorCodeIntakeFailed, Detail: "email cursor is not writable", Cause: err}
	}
	return nil
}

func (a *Adapter) ingestWindow(ctx context.Context, session Session, cursor *Cursor, first, last uint32) (Cursor, error) {
	infos, err := session.List(ctx, first, last)
	if err != nil {
		return *cursor, err
	}
	infos = slices.DeleteFunc(infos, func(info MessageInfo) bool { return info.UID < first || info.UID > last })
	slices.SortFunc(infos, func(x, y MessageInfo) int { return int(int64(x.UID) - int64(y.UID)) })
	infos = slices.CompactFunc(infos, func(x, y MessageInfo) bool { return x.UID == y.UID })
	progress := *cursor
	for _, info := range infos {
		if err := a.ingest(ctx, session, info); err != nil {
			return progress, err
		}
		progress.LastUID = info.UID
		if err := a.saveCursor(ctx, progress); err != nil {
			return progress, err
		}
	}
	progress.LastUID = last
	if err := a.saveCursor(ctx, progress); err != nil {
		return progress, err
	}
	return progress, nil
}

func classifyFault(err error) string {
	code, ok := CodeOf(err)
	if !ok {
		return faultUnreachable
	}
	switch code {
	case ErrorCodeCredentialsUnavailable:
		return faultPasswordUnresolved
	case ErrorCodeLoginRejected:
		return faultLoginRejected
	case ErrorCodeProtocolInvalid:
		return faultProtocol
	case ErrorCodeIntakeFailed:
		return faultIntake
	default:
		return faultUnreachable
	}
}

func (a *Adapter) retryDelay(attempt int) time.Duration {
	delay := a.retryBase << min(attempt, 8)
	if delay > a.retryCap || delay <= 0 {
		return a.retryCap
	}
	return delay
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

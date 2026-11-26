package app

import (
	"context"
	"database/sql"
	"time"

	turnstore "github.com/anggasct/aura/internal/platform/turn/store"
)

func appendSessionText(ctx context.Context, db *sql.DB, sessionID, id, author, text string, at time.Time) error {
	event, err := turnstore.RuntimeEventFromADK(sessionID, id, sessionEventWithText(id, author, text, at))
	if err != nil {
		return err
	}
	_, err = turnstore.NewEventStore(db).AppendSequenced(ctx, sessionID, &event)
	return err
}

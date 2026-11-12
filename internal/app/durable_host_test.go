package app

import (
	"database/sql"
	"testing"
	"time"

	"github.com/anggasct/aura/internal/config"
	"github.com/anggasct/aura/internal/store"
)

func openDatabaseFile(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := store.OpenDB(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("open the database file: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestDurableHostStopGraceFitsTheShutdownTimeout(t *testing.T) {
	cases := []struct {
		shutdown, grace time.Duration
	}{
		{30 * time.Second, 10 * time.Second},
		{10 * time.Second, 5 * time.Second},
		{time.Second, 500 * time.Millisecond},
		{time.Millisecond, time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.shutdown.String(), func(t *testing.T) {
			options := durableHostOptions(nil, nil, config.Runtime{ShutdownTimeout: config.Duration(tc.shutdown)})
			if options.StopGrace != tc.grace {
				t.Fatalf("stop grace for a shutdown timeout of %s = %s, want %s", tc.shutdown, options.StopGrace, tc.grace)
			}
		})
	}
}

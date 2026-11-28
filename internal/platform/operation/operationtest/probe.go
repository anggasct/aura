package operationtest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/anggasct/aura/internal/platform/operation"
	"github.com/anggasct/aura/internal/platform/principal"
)

func Snapshot(tb testing.TB, db *sql.DB) string {
	tb.Helper()
	ctx := tb.Context()
	names, err := tableNames(ctx, db)
	if err != nil {
		tb.Fatalf("list the tables: %v", err)
	}
	digest := sha256.New()
	for _, name := range names {
		rows, err := tableRows(ctx, db, name)
		if err != nil {
			tb.Fatalf("read the table %s: %v", name, err)
		}
		digest.Write([]byte(name + "\n" + strings.Join(rows, "\n") + "\n"))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func tableNames(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func tableRows(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	var query strings.Builder
	query.WriteString(`SELECT * FROM "`)
	query.WriteString(strings.ReplaceAll(table, `"`, `""`))
	query.WriteString(`"`)
	rows, err := db.QueryContext(ctx, query.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var lines []string
	for rows.Next() {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		lines = append(lines, fmt.Sprint(values...))
	}
	slices.Sort(lines)
	return lines, rows.Err()
}

type WriteProbe struct {
	Registry *operation.Registry
	DB       *sql.DB
	ID       string
	Input    json.RawMessage
	Outside  func() string
}

func RequireWritesOnlyInCommit(tb testing.TB, probe *WriteProbe) {
	tb.Helper()
	if problem := WritesOutsideCommit(tb, probe); problem != "" {
		tb.Error(problem)
	}
}

func WritesOutsideCommit(tb testing.TB, probe *WriteProbe) string {
	tb.Helper()
	newCall := func() *operation.Call {
		return &operation.Call{
			Caller:         operation.Caller{Surface: operation.SurfaceChat, PrincipalID: OwnerID, Class: principal.ClassOwner, SessionID: SessionID, Direct: true},
			Input:          probe.Input,
			IdempotencyKey: "write-probe-" + probe.ID,
			RequestDigest:  operation.Digest(probe.Input),
		}
	}
	first, err := probe.Registry.Invoke(tb.Context(), probe.ID, newCall())
	if err != nil {
		tb.Fatalf("%s: first call: %v", probe.ID, err)
	}
	before := Snapshot(tb, probe.DB)
	outsideBefore := probe.outside()
	replayed, err := probe.Registry.Invoke(tb.Context(), probe.ID, newCall())
	if err != nil {
		tb.Fatalf("%s: replayed call: %v", probe.ID, err)
	}
	if string(first.Data) != string(replayed.Data) {
		return fmt.Sprintf("%s: a replayed call returned %s, want the recorded %s", probe.ID, replayed.Data, first.Data)
	}
	if Snapshot(tb, probe.DB) != before {
		return probe.ID + ": a replayed call changed the database, so the handler writes outside Commit"
	}
	if probe.outside() != outsideBefore {
		return probe.ID + ": a replayed call reached a side effect outside the database, so the handler acts outside Commit"
	}
	return ""
}

func (p *WriteProbe) outside() string {
	if p.Outside == nil {
		return ""
	}
	return p.Outside()
}

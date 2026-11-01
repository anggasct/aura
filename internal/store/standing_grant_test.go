package store

import (
	"testing"
	"time"
)

func TestStandingGrantLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	grants := NewStandingGrantStore(db)
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	granted, err := grants.Grant(ctx, &StandingGrant{
		PrincipalID: "discord:111", AgentID: "kantor", ToolName: "write_file",
		GrantedBy: "discord:777", GrantedAt: now, ExpiresAt: now.Add(168 * time.Hour),
	})
	if err != nil || granted.ID == "" {
		t.Fatalf("Grant() = %+v, %v", granted, err)
	}
	if _, active, err := grants.Active(ctx, "discord:111", "kantor", "write_file", now.Add(time.Hour)); err != nil || !active {
		t.Fatalf("Active() = %v, %v; want active", active, err)
	}
	for name, lookup := range map[string][3]string{
		"other tool":      {"discord:111", "kantor", "exec"},
		"other principal": {"discord:222", "kantor", "write_file"},
		"other persona":   {"discord:111", "main", "write_file"},
	} {
		if _, active, err := grants.Active(ctx, lookup[0], lookup[1], lookup[2], now.Add(time.Hour)); err != nil || active {
			t.Errorf("%s: Active() = %v, %v; want inactive", name, active, err)
		}
	}
	if _, active, _ := grants.Active(ctx, "discord:111", "kantor", "write_file", now.Add(168*time.Hour)); active {
		t.Error("grant still active at its expiry")
	}
	listed, err := grants.List(ctx, "discord:111", now)
	if err != nil || len(listed) != 1 || listed[0].ID != granted.ID {
		t.Fatalf("List() = %+v, %v", listed, err)
	}
	if others, _ := grants.List(ctx, "discord:222", now); len(others) != 0 {
		t.Errorf("List(other principal) = %+v, want none", others)
	}
	if revoked, err := grants.Revoke(ctx, granted.ID, now.Add(2*time.Hour)); err != nil || !revoked {
		t.Fatalf("Revoke() = %v, %v", revoked, err)
	}
	if revoked, _ := grants.Revoke(ctx, granted.ID, now.Add(3*time.Hour)); revoked {
		t.Error("second Revoke() reported a change")
	}
	if _, active, _ := grants.Active(ctx, "discord:111", "kantor", "write_file", now.Add(4*time.Hour)); active {
		t.Error("revoked grant is still active")
	}
	if listed, _ := grants.List(ctx, "", now.Add(4*time.Hour)); len(listed) != 0 {
		t.Errorf("List() after revoke = %+v, want none", listed)
	}
}

func TestStandingGrantRejectsIncompleteGrants(t *testing.T) {
	grants := NewStandingGrantStore(newTestDB(t))
	now := time.Now().UTC()
	for name, grant := range map[string]StandingGrant{
		"no principal": {AgentID: "kantor", ToolName: "exec", GrantedBy: "discord:777", GrantedAt: now, ExpiresAt: now.Add(time.Hour)},
		"no granter":   {PrincipalID: "discord:111", AgentID: "kantor", ToolName: "exec", GrantedAt: now, ExpiresAt: now.Add(time.Hour)},
		"no lifetime":  {PrincipalID: "discord:111", AgentID: "kantor", ToolName: "exec", GrantedBy: "discord:777", GrantedAt: now, ExpiresAt: now},
	} {
		if _, err := grants.Grant(t.Context(), &grant); err == nil {
			t.Errorf("%s: Grant() succeeded", name)
		}
	}
}

func TestAccessSchemaTablesExist(t *testing.T) {
	db := newTestDB(t)
	for _, table := range []string{"principal_grant", "pairing_request", "standing_grant", "session_handoff"} {
		var name string
		if err := db.QueryRowContext(t.Context(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
	for _, column := range []string{"principal_id", "principal_class"} {
		var count int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_table_info('usage_reservation') WHERE name = ?`, column).Scan(&count); err != nil || count != 1 {
			t.Errorf("usage_reservation.%s = %d, %v", column, count, err)
		}
	}
}

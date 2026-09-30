package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// A v1 database (as deployed before alerts existed) upgrades in place and
// keeps its data.
func TestMigrateV1ToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.db")
	db, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range schemaV1 {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Exec(`INSERT INTO meta(key, value) VALUES ('schema_version', '1')`)
	db.Exec(`INSERT INTO nodes(id, name) VALUES (1, 'a')`)
	db.Exec(`INSERT INTO traffic_period(node, iface, start, rx, tx, ts) VALUES (1, 'eth0', '2026-09-01', 777, 1, 1)`)
	db.Close()

	s := open(t, path)
	defer s.Close()
	var v string
	s.w.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
	if v != "2" {
		t.Fatalf("schema_version = %s", v)
	}
	p, err := s.Periods(ctx, "a", 1)
	if err != nil || len(p) != 1 || p[0].RX != 777 {
		t.Fatalf("v1 data after upgrade: %+v %v", p, err)
	}
	if err := s.SaveAlerts([]AlertState{{Rule: "r", Node: "a", State: "firing"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.db")
	s := open(t, path)
	s.w.Exec(`UPDATE meta SET value = '99' WHERE key = 'schema_version'`)
	s.Close()
	if _, err := Open(path, Options{Location: sh, Retention: ret, Log: discard}); err == nil {
		t.Fatal("opened a database from a newer version")
	}
}

func TestAlertStateAndHistory(t *testing.T) {
	s := newStore(t)
	now := time.Now().Unix()
	fire := AlertState{Rule: "cpu", Node: "a", State: "firing", Since: now, Notified: now, Value: 95}
	quota := AlertState{Rule: "quota", Node: "a", Target: "2026-09-01", State: "level", Value: 80}
	err := s.SaveAlerts([]AlertState{fire, quota, {Rule: "gone", Node: "a", State: "firing"}}, nil, []AlertEvent{
		{TS: now - 10, Rule: "cpu", Node: "a", Event: "firing", Value: 95, Message: "old"},
		{TS: now, Rule: "cpu", Node: "a", Event: "recovered", Message: "new"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PruneAlertStates([]string{"cpu", "quota"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAlerts(nil, []AlertState{fire}, nil); err != nil {
		t.Fatal(err)
	}
	st, err := s.AlertStates()
	if err != nil || len(st) != 1 || st[0] != quota {
		t.Fatalf("states: %+v %v", st, err)
	}
	h, err := s.AlertHistory(ctx, now-60, now+1, 10)
	if err != nil || len(h) != 2 || h[0].Message != "new" {
		t.Fatalf("history: %+v %v", h, err)
	}
}

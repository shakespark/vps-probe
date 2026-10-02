package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A v1 database (as deployed before alerts existed) upgrades in place and
// keeps its data.
func TestMigrateV1ToLatest(t *testing.T) {
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
	db.Exec(`INSERT INTO node_status(node, max_ts, fresh_at, skew) VALUES (1, 100, 100, 0)`)
	db.Close()

	s := open(t, path)
	defer s.Close()
	var v string
	s.w.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
	if v != strconv.Itoa(schemaVersion) {
		t.Fatalf("schema_version = %s", v)
	}
	s.SyncNodes([]string{"a"})
	if st, err := s.Status(ctx, "a"); err != nil || st == nil || st.MaxTS != 100 || st.IP != "" {
		t.Fatalf("v1 status after upgrade: %+v %v", st, err)
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
	h, err := s.AlertHistory(ctx, now-60, now+1, AlertFilter{}, 10)
	if err != nil || len(h) != 2 || h[0].Message != "new" {
		t.Fatalf("history: %+v %v", h, err)
	}
}

func TestAlertHistoryFilter(t *testing.T) {
	s := newStore(t)
	now := time.Now().Unix()
	err := s.SaveAlerts(nil, nil, []AlertEvent{
		{TS: now - 30, Rule: "cpu", Node: "a", Event: "firing", Message: "1"},
		{TS: now - 20, Rule: "cpu", Node: "a", Event: "repeat", Message: "2"},
		{TS: now - 10, Rule: "offline", Node: "b", Event: "firing", Message: "3"},
		{TS: now, Rule: "cpu", Node: "a", Event: "recovered", Message: "4"},
		{TS: now - 7200, Rule: "disk", Node: "c", Event: "firing", Message: "old"},
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs := func(f AlertFilter) string {
		h, err := s.AlertHistory(ctx, now-60, now+1, f, 10)
		if err != nil {
			t.Fatal(err)
		}
		var m []string
		for _, e := range h {
			m = append(m, e.Message)
		}
		return strings.Join(m, ",")
	}
	for _, c := range []struct {
		f    AlertFilter
		want string
	}{
		{AlertFilter{}, "4,3,2,1"},
		{AlertFilter{Node: "a"}, "4,2,1"},
		{AlertFilter{Rule: "offline"}, "3"},
		{AlertFilter{Events: []string{"firing", "repeat"}}, "3,2,1"},
		{AlertFilter{Node: "a", Events: []string{"firing"}}, "1"},
		{AlertFilter{Node: "zz"}, ""},
	} {
		if got := msgs(c.f); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.f, got, c.want)
		}
	}
	nodes, rules, err := s.AlertFacets(ctx, now-60, now+1)
	if err != nil || strings.Join(nodes, ",") != "a,b" || strings.Join(rules, ",") != "cpu,offline" {
		t.Fatalf("facets %v %v %v", nodes, rules, err)
	}
}

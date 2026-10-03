package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A database written by 0.1.x is refused with a message, not read wrongly.
func TestRefusesOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.db")
	db, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	db.Exec(`INSERT INTO meta(key, value) VALUES ('schema_version', '5')`)
	db.Close()
	if _, err := Open(path, Options{Location: sh, Retention: ret, Log: discard}); err == nil || !strings.Contains(err.Error(), "0.1.x") {
		t.Fatalf("opened a 0.1.x database: %v", err)
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
	path := filepath.Join(t.TempDir(), "probe.db")
	s := open(t, path)
	now := time.Now().Unix()
	fire := AlertState{Rule: "cpu", Node: "a", Firing: true, Since: now, Notified: now, Value: 95}
	pending := AlertState{Rule: "disk", Node: "a", Target: "/", Since: now, Value: 91}
	quota := ReportMark{Report: "traffic_quota", Node: "a", Mark: "1788192000", Level: 80, At: now}
	err := s.SaveAlerts(AlertChanges{Put: []AlertState{fire, pending}, Marks: []ReportMark{quota}, Events: []AlertEvent{
		{TS: now - 10, Rule: "cpu", Node: "a", Event: "firing", Value: "95.0%", Message: "old"},
		{TS: now, Rule: "cpu", Node: "a", Event: "recovered", Message: "new"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	quota.Level = 90 // a mark is replaced, not added to
	if err := s.SaveAlerts(AlertChanges{Del: []AlertState{fire}, Marks: []ReportMark{quota}}); err != nil {
		t.Fatal(err)
	}
	// Survives a restart.
	s.Close()
	s = open(t, path)
	defer s.Close()
	st, marks, err := s.AlertStates()
	if err != nil || len(st) != 1 || st[0] != pending || len(marks) != 1 || marks[0] != quota {
		t.Fatalf("states: %+v, marks: %+v, %v", st, marks, err)
	}
	h, err := s.AlertHistory(ctx, now-60, now+1, AlertFilter{}, 10)
	if err != nil || len(h) != 2 || h[0].Message != "new" || h[1].Value != "95.0%" {
		t.Fatalf("history: %+v %v", h, err)
	}
}

func TestAlertHistoryFilter(t *testing.T) {
	s := newStore(t)
	now := time.Now().Unix()
	err := s.SaveAlerts(AlertChanges{Events: []AlertEvent{
		{TS: now - 30, Rule: "cpu", Node: "a", Event: "firing", Message: "1"},
		{TS: now - 20, Rule: "cpu", Node: "a", Event: "repeat", Message: "2"},
		{TS: now - 10, Rule: "offline", Node: "b", Event: "firing", Message: "3"},
		{TS: now, Rule: "cpu", Node: "a", Event: "recovered", Message: "4"},
		{TS: now - 7200, Rule: "disk", Node: "c", Event: "firing", Message: "old"},
	}})
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

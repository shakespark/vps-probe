package store

import (
	"context"
	"database/sql"
	"strings"
)

// AlertState is an alert whose condition holds: pending until it has held
// for the rule's time, firing after it was notified.
type AlertState struct {
	Rule     string
	Node     string
	Target   string // a mount point or peer name, or ""
	Firing   bool
	Since    int64 // the condition has held since
	Notified int64 // when the last message about it was sent
	Value    float64
}

// ReportMark is what a report last sent for a node ("" for one that covers
// all nodes): the thing it was about, such as a period start, an expiry date
// or an IP, and for reports with levels the level reached.
type ReportMark struct {
	Report string
	Node   string
	Mark   string
	Level  float64
	At     int64
}

// AlertEvent is one line of alert history.
type AlertEvent struct {
	TS      int64  `json:"ts"`
	Rule    string `json:"rule"` // an alert rule's name or a report's type
	Node    string `json:"node"` // "" for a report covering all nodes
	Target  string `json:"target"`
	Event   string `json:"event"` // firing | repeat | recovered | notice | report
	Value   string `json:"value"` // as shown, e.g. "95.2%"
	Message string `json:"message"`
}

// AlertChanges is the outcome of one evaluation round.
type AlertChanges struct {
	Put    []AlertState // new or changed
	Del    []AlertState // over; only rule, node and target are used
	Marks  []ReportMark
	Events []AlertEvent
}

func (c *AlertChanges) empty() bool { return len(c.Put)+len(c.Del)+len(c.Marks)+len(c.Events) == 0 }

// AlertStates returns what SaveAlerts stored: the alerts in progress and
// the reports' marks.
func (s *Store) AlertStates() ([]AlertState, []ReportMark, error) {
	var states []AlertState
	rows, err := s.w.Query(`SELECT rule, node, target, firing, since, notified, value FROM alert_state`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a AlertState
		if err := rows.Scan(&a.Rule, &a.Node, &a.Target, &a.Firing, &a.Since, &a.Notified, &a.Value); err != nil {
			return nil, nil, err
		}
		states = append(states, a)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var marks []ReportMark
	rows, err = s.w.Query(`SELECT report, node, mark, level, at FROM report_state`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m ReportMark
		if err := rows.Scan(&m.Report, &m.Node, &m.Mark, &m.Level, &m.At); err != nil {
			return nil, nil, err
		}
		marks = append(marks, m)
	}
	return states, marks, rows.Err()
}

// SaveAlerts applies one evaluation round atomically.
func (s *Store) SaveAlerts(c AlertChanges) error {
	if c.empty() {
		return nil
	}
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, a := range c.Put {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO alert_state(rule, node, target, firing, since, notified, value)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, a.Rule, a.Node, a.Target, a.Firing, a.Since, a.Notified, a.Value); err != nil {
			return err
		}
	}
	for _, a := range c.Del {
		if _, err := tx.Exec(`DELETE FROM alert_state WHERE rule = ? AND node = ? AND target = ?`,
			a.Rule, a.Node, a.Target); err != nil {
			return err
		}
	}
	for _, m := range c.Marks {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO report_state(report, node, mark, level, at) VALUES (?, ?, ?, ?, ?)`,
			m.Report, m.Node, m.Mark, m.Level, m.At); err != nil {
			return err
		}
	}
	for _, e := range c.Events {
		if _, err := tx.Exec(`INSERT INTO alert_history(ts, rule, node, target, event, value, message)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, e.TS, e.Rule, e.Node, e.Target, e.Event, e.Value, e.Message); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AlertFilter narrows AlertHistory. Empty fields match everything.
type AlertFilter struct {
	Node   string
	Rule   string
	Events []string // any of these
}

// AlertHistory returns events in [from, to) that match f, newest first.
func (s *Store) AlertHistory(ctx context.Context, from, to int64, f AlertFilter, limit int) ([]AlertEvent, error) {
	q := `SELECT ts, rule, node, target, event, value, message FROM alert_history
		WHERE ts >= ? AND ts < ?`
	args := []any{from, to}
	if f.Node != "" {
		q += ` AND node = ?`
		args = append(args, f.Node)
	}
	if f.Rule != "" {
		q += ` AND rule = ?`
		args = append(args, f.Rule)
	}
	if len(f.Events) > 0 {
		q += ` AND event IN (?` + strings.Repeat(", ?", len(f.Events)-1) + `)`
		for _, e := range f.Events {
			args = append(args, e)
		}
	}
	q += ` ORDER BY ts DESC, id DESC LIMIT ?`
	args = append(args, limit)
	out := []AlertEvent{}
	err := s.each(ctx, q, args, func(r *sql.Rows) error {
		var e AlertEvent
		if err := r.Scan(&e.TS, &e.Rule, &e.Node, &e.Target, &e.Event, &e.Value, &e.Message); err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

// AlertFacets returns the distinct nodes and rules with events in
// [from, to), sorted, for the history filter menus. It includes rules no
// longer configured, so their old events stay reachable.
func (s *Store) AlertFacets(ctx context.Context, from, to int64) (nodes, rules []string, err error) {
	nodes, rules = []string{}, []string{}
	for _, c := range []struct {
		col string
		out *[]string
	}{{"node", &nodes}, {"rule", &rules}} {
		// node is empty for events covering all nodes.
		err = s.each(ctx, `SELECT DISTINCT `+c.col+` FROM alert_history WHERE ts >= ? AND ts < ? AND `+c.col+` != '' ORDER BY 1`,
			[]any{from, to}, func(r *sql.Rows) error {
				var v string
				if err := r.Scan(&v); err != nil {
					return err
				}
				*c.out = append(*c.out, v)
				return nil
			})
		if err != nil {
			return nil, nil, err
		}
	}
	return nodes, rules, nil
}

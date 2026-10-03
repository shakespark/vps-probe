package store

import (
	"context"
	"database/sql"
	"strings"
)

// AlertState is a persisted alert instance: a firing alert, or for traffic
// rules the highest quota level already notified in a period.
type AlertState struct {
	Rule     string  `json:"rule"`
	Node     string  `json:"node"`
	Target   string  `json:"target"` // mount, peer name, period start, expiry date, IP, or ""
	State    string  `json:"state"`
	Since    int64   `json:"since"`
	Notified int64   `json:"notified"`
	Value    float64 `json:"value"`
}

// AlertEvent is one line of alert history.
type AlertEvent struct {
	TS      int64   `json:"ts"`
	Rule    string  `json:"rule"`
	Node    string  `json:"node"`
	Target  string  `json:"target"`
	Event   string  `json:"event"` // firing | repeat | recovered | level | changed | report
	Value   float64 `json:"value"`
	Message string  `json:"message"`
}

func (s *Store) AlertStates() ([]AlertState, error) {
	rows, err := s.w.Query(`SELECT rule, node, target, state, since, notified, COALESCE(value, 0) FROM alert_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertState
	for rows.Next() {
		var a AlertState
		if err := rows.Scan(&a.Rule, &a.Node, &a.Target, &a.State, &a.Since, &a.Notified, &a.Value); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SaveAlerts applies one evaluation round atomically: states to upsert,
// states to delete, and history to append.
func (s *Store) SaveAlerts(put []AlertState, del []AlertState, events []AlertEvent) error {
	if len(put)+len(del)+len(events) == 0 {
		return nil
	}
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, a := range put {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO alert_state(rule, node, target, state, since, notified, value)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, a.Rule, a.Node, a.Target, a.State, a.Since, a.Notified, a.Value); err != nil {
			return err
		}
	}
	for _, a := range del {
		if _, err := tx.Exec(`DELETE FROM alert_state WHERE rule = ? AND node = ? AND target = ?`,
			a.Rule, a.Node, a.Target); err != nil {
			return err
		}
	}
	for _, e := range events {
		if _, err := tx.Exec(`INSERT INTO alert_history(ts, rule, node, target, event, value, message)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, e.TS, e.Rule, e.Node, e.Target, e.Event, e.Value, e.Message); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneAlertStates drops states of rules no longer in the config.
func (s *Store) PruneAlertStates(rules []string) error {
	if len(rules) == 0 {
		_, err := s.w.Exec(`DELETE FROM alert_state`)
		return err
	}
	args := make([]any, len(rules))
	for i, r := range rules {
		args[i] = r
	}
	_, err := s.w.Exec(`DELETE FROM alert_state WHERE rule NOT IN (?`+strings.Repeat(", ?", len(rules)-1)+`)`, args...)
	return err
}

// AlertFilter narrows AlertHistory. Empty fields match everything.
type AlertFilter struct {
	Node   string
	Rule   string
	Events []string // any of these
}

// AlertHistory returns events in [from, to) that match f, newest first.
func (s *Store) AlertHistory(ctx context.Context, from, to int64, f AlertFilter, limit int) ([]AlertEvent, error) {
	q := `SELECT ts, rule, node, target, event, COALESCE(value, 0), message FROM alert_history
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
		// node is empty for fleet-wide events (weekly_report).
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

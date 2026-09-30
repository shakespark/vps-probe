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
	Target   string  `json:"target"` // mount, peer name, period start, or ""
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
	Event   string  `json:"event"` // firing | repeat | recovered | level
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

// AlertHistory returns events in [from, to), newest first.
func (s *Store) AlertHistory(ctx context.Context, from, to int64, limit int) ([]AlertEvent, error) {
	out := []AlertEvent{}
	err := s.each(ctx, `SELECT ts, rule, node, target, event, COALESCE(value, 0), message FROM alert_history
		WHERE ts >= ? AND ts < ? ORDER BY ts DESC, id DESC LIMIT ?`, []any{from, to, limit}, func(r *sql.Rows) error {
		var e AlertEvent
		if err := r.Scan(&e.TS, &e.Rule, &e.Node, &e.Target, &e.Event, &e.Value, &e.Message); err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

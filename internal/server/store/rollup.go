package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Rollup recomputes every 5m and 1h bucket that starts at or after since.
// The first bucket is rounded up, never down: its raw rows may already be
// partly deleted by retention, and recomputing it would shrink it. Buckets
// are keyed by report ts, not arrival time, so late backlog lands in the
// right bucket once the window covers it. INSERT OR REPLACE makes reruns
// harmless.
func (s *Store) Rollup(since time.Time) error {
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, step := range []int64{step5m, step1h} {
		from := (since.Unix() + step - 1) / step * step
		suffix := "_5m"
		if step == step1h {
			suffix = "_1h"
		}
		stmts := []string{
			`INSERT OR REPLACE INTO metrics` + suffix + ` (node, ts, cpu, cpu_max, steal, steal_max, softirq, softirq_max,
				load1, load1_max, load5, load15, mem_total, mem_used, mem_used_max, swap_total, swap_used, swap_used_max,
				tcp, tcp_max, udp, udp_max, tcp_tw, threads, threads_max)
			SELECT node, ts / :step * :step, avg(cpu), max(cpu), avg(steal), max(steal), avg(softirq), max(softirq),
				avg(load1), max(load1), avg(load5), avg(load15),
				max(mem_total), avg(mem_used), max(mem_used), max(swap_total), avg(swap_used), max(swap_used),
				avg(tcp), max(tcp), avg(udp), max(udp), avg(tcp_tw), avg(threads), max(threads)
			FROM metrics_raw WHERE ts >= :from GROUP BY node, ts / :step`,

			`INSERT OR REPLACE INTO net` + suffix + ` (node, iface, ts, rx, rx_max, tx, tx_max,
				rx_pps, rx_pps_max, tx_pps, tx_pps_max)
			SELECT node, iface, ts / :step * :step, avg(rx), max(rx), avg(tx), max(tx),
				avg(rx_pps), max(rx_pps), avg(tx_pps), max(tx_pps)
			FROM net_raw WHERE ts >= :from GROUP BY node, iface, ts / :step`,

			// avg is weighted by replies received; min/max/avg ignore rows
			// where nothing came back (stored as NULL).
			`INSERT OR REPLACE INTO ping` + suffix + ` (src, dst, ts, sent, lost, min, avg, max, jitter)
			SELECT src, dst, ts / :step * :step, sum(sent), sum(lost), min(min), ` + pingAvg + `, max(max), avg(jitter)
			FROM ping_raw WHERE ts >= :from GROUP BY src, dst, ts / :step`,
		}
		if step == step1h {
			stmts = append(stmts, `INSERT OR REPLACE INTO disk_1h (node, mount, ts, total, used, used_max, avail, inode_pct)
			SELECT node, mount, ts / :step * :step, max(total), avg(used), max(used), avg(avail), avg(inode_pct)
			FROM disk_raw WHERE ts >= :from GROUP BY node, mount, ts / :step`)
		}
		for _, q := range stmts {
			if _, err := tx.Exec(q, sql.Named("step", step), sql.Named("from", from)); err != nil {
				return fmt.Errorf("rollup: %w", err)
			}
		}
	}
	return tx.Commit()
}

// Cleanup deletes data older than the retention windows; alert history
// follows the longest one. Traffic totals and node status are kept forever.
func (s *Store) Cleanup() error {
	now, r := s.now(), s.ret
	raw, m5, h1 := now.Add(-r.Raw).Unix(), now.Add(-r.M5).Unix(), now.Add(-r.H1).Unix()
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for table, cut := range map[string]int64{
		"metrics_raw": raw, "net_raw": raw, "disk_raw": raw, "ping_raw": raw,
		"metrics_5m": m5, "net_5m": m5, "ping_5m": m5,
		"metrics_1h": h1, "net_1h": h1, "ping_1h": h1, "disk_1h": h1, "alert_history": h1,
	} {
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE ts < ?", cut); err != nil {
			return fmt.Errorf("cleanup %s: %w", table, err)
		}
	}
	return tx.Commit()
}

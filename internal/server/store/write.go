package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"time"

	pb "github.com/shakespark/vps-probe/internal/proto/probev1"
)

// Write stores one (already validated) report piece in a single transaction.
// Every statement is idempotent, so duplicates and replays change nothing,
// and pieces of a split report sharing one ts merge into the same rows.
// from is the packet's source address; it is recorded only when the report
// is the node's newest.
func (s *Store) Write(node string, rep *pb.Report, from netip.Addr, arrival time.Time) error {
	rid, ok := s.nodeID(node)
	if !ok {
		return fmt.Errorf("store: unknown node %q", node)
	}
	tx, err := s.w.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := rep.Ts

	res, err := tx.Exec(`INSERT INTO node_status(node, max_ts, fresh_at, skew) VALUES (?, ?, ?, ?)
		ON CONFLICT(node) DO UPDATE SET max_ts = excluded.max_ts, fresh_at = excluded.fresh_at, skew = excluded.skew
		WHERE excluded.max_ts > node_status.max_ts`,
		rid, ts, arrival.Unix(), arrival.Unix()-ts)
	if err != nil {
		return err
	}
	if fresh, err := res.RowsAffected(); err != nil {
		return err
	} else if fresh > 0 && from.IsValid() {
		if _, err := tx.Exec(`INSERT INTO node_addr(node, ip, since) VALUES (?, ?, ?)
			ON CONFLICT(node) DO UPDATE SET ip = excluded.ip, since = excluded.since WHERE ip <> excluded.ip`,
			rid, from.Unmap().String(), arrival.Unix()); err != nil {
			return err
		}
	}
	if rep.Sys != nil {
		y := rep.Sys
		js, err := json.Marshal(SysInfo{Hostname: y.Hostname, OS: y.Os, Kernel: y.Kernel, Arch: y.Arch,
			Cores: y.Cores, BootTime: y.BootTime, Uptime: y.Uptime, AgentVersion: y.AgentVersion})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE node_status SET sys = ?, sys_ts = ?
			WHERE node = ? AND (sys_ts IS NULL OR sys_ts <= ?)`, string(js), ts, rid, ts); err != nil {
			return err
		}
	}

	if rep.Cpu != nil || rep.Load != nil || rep.Mem != nil || rep.Sockets != nil {
		var cpu, steal, softirq, l1, l5, l15 sql.NullFloat64
		var mt, mu, st, su, tcp, udp, tw, threads sql.NullInt64
		if c := rep.Cpu; c != nil {
			cpu, steal = nf(c.Usage), nf(c.Steal)
			if c.Softirq != nil { // unset: an agent before 0.1.14
				softirq = nf(*c.Softirq)
			}
		}
		if l := rep.Load; l != nil {
			l1, l5, l15 = nf(l.L1), nf(l.L5), nf(l.L15)
			if l.Threads > 0 { // 0: an agent before 0.1.9
				threads = ni(uint64(l.Threads))
			}
		}
		if m := rep.Mem; m != nil {
			mt, mu, st, su = ni(m.Total), ni(m.Used), ni(m.SwapTotal), ni(m.SwapUsed)
		}
		if k := rep.Sockets; k != nil {
			tcp, udp, tw = ni(uint64(k.Tcp)), ni(uint64(k.Udp)), ni(uint64(k.TcpTw))
		}
		if _, err := tx.Exec(`INSERT INTO metrics_raw(node, ts, cpu, steal, softirq, load1, load5, load15,
				mem_total, mem_used, swap_total, swap_used, tcp, udp, tcp_tw, threads)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(node, ts) DO UPDATE SET
				cpu = COALESCE(excluded.cpu, cpu), steal = COALESCE(excluded.steal, steal),
				softirq = COALESCE(excluded.softirq, softirq),
				load1 = COALESCE(excluded.load1, load1), load5 = COALESCE(excluded.load5, load5),
				load15 = COALESCE(excluded.load15, load15),
				mem_total = COALESCE(excluded.mem_total, mem_total), mem_used = COALESCE(excluded.mem_used, mem_used),
				swap_total = COALESCE(excluded.swap_total, swap_total), swap_used = COALESCE(excluded.swap_used, swap_used),
				tcp = COALESCE(excluded.tcp, tcp), udp = COALESCE(excluded.udp, udp),
				tcp_tw = COALESCE(excluded.tcp_tw, tcp_tw), threads = COALESCE(excluded.threads, threads)`,
			rid, ts, cpu, steal, softirq, l1, l5, l15, mt, mu, st, su, tcp, udp, tw, threads); err != nil {
			return err
		}
	}

	for _, n := range rep.Net {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO net_raw(node, iface, ts, rx, tx, rx_pps, tx_pps) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			rid, n.Iface, ts, float64(n.RxRate), float64(n.TxRate), optf(n.RxPps), optf(n.TxPps)); err != nil {
			return err
		}
	}
	for _, d := range rep.Disks {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO disk_raw(node, mount, ts, total, used, avail, inode_pct)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, rid, d.Mount, ts, int64(d.Total), int64(d.Used), int64(d.Avail), nf(d.InodePct)); err != nil {
			return err
		}
	}
	for _, p := range rep.Pings {
		var mn, avg, mx, jit sql.NullFloat64
		if p.Sent > p.Lost {
			mn, avg, mx, jit = nf(p.Min), nf(p.Avg), nf(p.Max), nf(p.Jitter)
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO ping_raw(src, dst, ts, sent, lost, min, avg, max, jitter)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, rid, p.Target, ts, p.Sent, p.Lost, mn, avg, mx, jit); err != nil {
			return err
		}
	}

	day := time.Unix(ts, 0).In(s.loc).Format(time.DateOnly)
	for _, t := range rep.Traffic {
		if c := t.Cur; c != nil {
			if err := upsertPeriod(tx, rid, t.Iface, c, ts); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO traffic_daily(node, iface, day, start, rx, tx, ts) VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(node, iface, day) DO UPDATE SET start = excluded.start, rx = excluded.rx, tx = excluded.tx, ts = excluded.ts
				WHERE excluded.ts > traffic_daily.ts`,
				rid, t.Iface, day, c.Start, int64(c.Rx), int64(c.Tx), ts); err != nil {
				return err
			}
		}
		// An empty previous period is usually just "no data"; skipping it
		// also keeps an agent that lost its state file from zeroing the
		// last month's record.
		if p := t.Prev; p != nil && (p.Rx > 0 || p.Tx > 0) {
			if err := upsertPeriod(tx, rid, t.Iface, p, ts); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func upsertPeriod(tx *sql.Tx, rid int64, iface string, p *pb.Period, ts int64) error {
	_, err := tx.Exec(`INSERT INTO traffic_period(node, iface, start, rx, tx, ts) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(node, iface, start) DO UPDATE SET rx = excluded.rx, tx = excluded.tx, ts = excluded.ts
		WHERE excluded.ts > traffic_period.ts`,
		rid, iface, p.Start, int64(p.Rx), int64(p.Tx), ts)
	return err
}

// nf drops float32 noise (12.3 would otherwise read back as 12.30000019).
func nf(v float32) sql.NullFloat64 {
	return sql.NullFloat64{Float64: math.Round(float64(v)*1000) / 1000, Valid: true}
}
func ni(v uint64) sql.NullInt64 { return sql.NullInt64{Int64: int64(v), Valid: true} }

// optf is an optional counter: NULL when the agent didn't send it.
func optf(v *uint64) sql.NullFloat64 {
	if v == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: float64(*v), Valid: true}
}

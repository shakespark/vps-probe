package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MaxPoints caps how many buckets one series returns; longer ranges are
// grouped into wider buckets in SQL.
const MaxPoints = 1000

// Status is a node's latest known state.
type Status struct {
	MaxTS   int64    `json:"max_ts"`     // newest report ts (agent clock)
	FreshAt int64    `json:"fresh_at"`   // server time that report arrived
	Skew    int64    `json:"clock_skew"` // server minus agent clock, seconds
	Sys     *SysInfo `json:"sys"`
	IP      string   `json:"ip,omitempty"`       // source address of the newest report
	IPSince int64    `json:"ip_since,omitempty"` // first report from it

	MetricsTS int64    `json:"metrics_ts,omitempty"`
	CPU       *float64 `json:"cpu"`
	Steal     *float64 `json:"steal"`
	SoftIRQ   *float64 `json:"softirq"` // null before agent 0.1.14
	Load1     *float64 `json:"load1"`
	Load5     *float64 `json:"load5"`
	Load15    *float64 `json:"load15"`
	MemTotal  *int64   `json:"mem_total"`
	MemUsed   *int64   `json:"mem_used"`
	SwapTotal *int64   `json:"swap_total"`
	SwapUsed  *int64   `json:"swap_used"`
	TCP       *int64   `json:"tcp"` // sockets and threads: null before agent 0.1.9
	UDP       *int64   `json:"udp"`
	TCPTW     *int64   `json:"tcp_tw"`
	Threads   *int64   `json:"threads"`

	Net     []IfaceRate `json:"net"`
	Disks   []Disk      `json:"disks"`
	Traffic *Period     `json:"traffic"` // current period
}

type SysInfo struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Kernel       string `json:"kernel"`
	Arch         string `json:"arch"`
	Cores        uint32 `json:"cores"`
	BootTime     int64  `json:"boot_time"`
	Uptime       uint64 `json:"uptime"` // as of the report that carried it
	AgentVersion string `json:"agent_version"`
}

type IfaceRate struct {
	Iface string  `json:"iface"`
	RX    float64 `json:"rx"` // bytes/s
	TX    float64 `json:"tx"`
}

type Disk struct {
	Mount    string  `json:"mount"`
	Total    int64   `json:"total"`
	Used     int64   `json:"used"`
	Avail    int64   `json:"avail"`
	InodePct float64 `json:"inode_pct"`
}

type Period struct {
	Start  string        `json:"start"`
	RX     int64         `json:"rx"`
	TX     int64         `json:"tx"`
	Ifaces []IfaceTotals `json:"ifaces"`
}

type IfaceTotals struct {
	Iface string `json:"iface"`
	RX    int64  `json:"rx"`
	TX    int64  `json:"tx"`
}

// Status returns the latest state of a node, or nil if it never reported.
func (s *Store) Status(ctx context.Context, node string) (*Status, error) {
	rid, ok := s.nodeID(node)
	if !ok {
		return nil, nil
	}
	st := &Status{}
	var sys, ip sql.NullString
	var ipSince sql.NullInt64
	err := s.r.QueryRowContext(ctx, `SELECT s.max_ts, s.fresh_at, s.skew, s.sys, a.ip, a.since
		FROM node_status s LEFT JOIN node_addr a ON a.node = s.node WHERE s.node = ?`, rid).
		Scan(&st.MaxTS, &st.FreshAt, &st.Skew, &sys, &ip, &ipSince)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	st.IP, st.IPSince = ip.String, ipSince.Int64
	if sys.Valid {
		st.Sys = &SysInfo{}
		if err := json.Unmarshal([]byte(sys.String), st.Sys); err != nil {
			return nil, err
		}
	}

	err = s.r.QueryRowContext(ctx, `SELECT ts, cpu, steal, softirq, load1, load5, load15, mem_total, mem_used, swap_total, swap_used,
			tcp, udp, tcp_tw, threads
		FROM metrics_raw WHERE node = ? ORDER BY ts DESC LIMIT 1`, rid).
		Scan(&st.MetricsTS, &st.CPU, &st.Steal, &st.SoftIRQ, &st.Load1, &st.Load5, &st.Load15,
			&st.MemTotal, &st.MemUsed, &st.SwapTotal, &st.SwapUsed, &st.TCP, &st.UDP, &st.TCPTW, &st.Threads)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	st.Net = []IfaceRate{}
	if err := s.each(ctx, `SELECT iface, rx, tx FROM net_raw
		WHERE node = ?1 AND ts = (SELECT max(ts) FROM net_raw WHERE node = ?1) ORDER BY iface`,
		[]any{rid}, func(r *sql.Rows) error {
			var n IfaceRate
			if err := r.Scan(&n.Iface, &n.RX, &n.TX); err != nil {
				return err
			}
			st.Net = append(st.Net, n)
			return nil
		}); err != nil {
		return nil, err
	}

	// Latest row per mount, so a mount that stopped reporting still shows
	// its last value rather than vanishing.
	st.Disks = []Disk{}
	if err := s.each(ctx, `SELECT d.mount, d.total, d.used, d.avail, d.inode_pct FROM disk_raw d
		JOIN (SELECT mount, max(ts) AS ts FROM disk_raw WHERE node = ?1 GROUP BY mount) m
		ON d.mount = m.mount AND d.ts = m.ts WHERE d.node = ?1 ORDER BY d.mount`,
		[]any{rid}, func(r *sql.Rows) error {
			var d Disk
			if err := r.Scan(&d.Mount, &d.Total, &d.Used, &d.Avail, &d.InodePct); err != nil {
				return err
			}
			st.Disks = append(st.Disks, d)
			return nil
		}); err != nil {
		return nil, err
	}

	periods, err := s.Periods(ctx, node, 1)
	if err != nil {
		return nil, err
	}
	if len(periods) > 0 {
		st.Traffic = &periods[0]
	}
	return st, nil
}

// Periods returns up to limit billing periods of a node, newest first, each
// summed over interfaces.
func (s *Store) Periods(ctx context.Context, node string, limit int) ([]Period, error) {
	rid, ok := s.nodeID(node)
	if !ok {
		return nil, nil
	}
	out := []Period{}
	err := s.each(ctx, `SELECT start, iface, rx, tx FROM traffic_period
		WHERE node = ?1 AND start IN (SELECT DISTINCT start FROM traffic_period WHERE node = ?1 ORDER BY start DESC LIMIT ?2)
		ORDER BY start DESC, iface`, []any{rid, limit}, func(r *sql.Rows) error {
		var start string
		var t IfaceTotals
		if err := r.Scan(&start, &t.Iface, &t.RX, &t.TX); err != nil {
			return err
		}
		if len(out) == 0 || out[len(out)-1].Start != start {
			out = append(out, Period{Start: start})
		}
		p := &out[len(out)-1]
		p.Ifaces = append(p.Ifaces, t)
		p.RX += t.RX
		p.TX += t.TX
		return nil
	})
	return out, err
}

type DayTraffic struct {
	Day string `json:"day"`
	RX  int64  `json:"rx"`
	TX  int64  `json:"tx"`
}

// Daily returns per-day usage within one period: each day's last seen total
// minus the previous day's, per interface, then summed. A day whose total
// went down (the agent's state was rebuilt) counts its own total.
func (s *Store) Daily(ctx context.Context, node, start string) ([]DayTraffic, error) {
	rid, ok := s.nodeID(node)
	if !ok {
		return nil, nil
	}
	byDay := map[string]*DayTraffic{}
	var lastIface string
	var prevRX, prevTX int64
	err := s.each(ctx, `SELECT iface, day, rx, tx FROM traffic_daily WHERE node = ? AND start = ? ORDER BY iface, day`,
		[]any{rid, start}, func(r *sql.Rows) error {
			var iface, day string
			var rx, tx int64
			if err := r.Scan(&iface, &day, &rx, &tx); err != nil {
				return err
			}
			if iface != lastIface {
				lastIface, prevRX, prevTX = iface, 0, 0
			}
			d := byDay[day]
			if d == nil {
				d = &DayTraffic{Day: day}
				byDay[day] = d
			}
			d.RX += delta(rx, prevRX)
			d.TX += delta(tx, prevTX)
			prevRX, prevTX = rx, tx
			return nil
		})
	if err != nil {
		return nil, err
	}
	out := make([]DayTraffic, 0, len(byDay))
	for _, d := range byDay {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out, nil
}

func delta(cur, prev int64) int64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

// Series is column-oriented: TS[i] pairs with Cols[name][i]. Missing
// values are null in JSON.
type Series struct {
	Tier string                `json:"tier"`
	Step int64                 `json:"step"`
	TS   []int64               `json:"ts"`
	Cols map[string][]*float64 `json:"cols"`
}

type column struct {
	name, raw, rollup string // aggregate over raw rows / over rollup rows
}

var metricCols = []column{
	{"cpu", "avg(cpu)", "avg(cpu)"},
	{"cpu_max", "max(cpu)", "max(cpu_max)"},
	{"steal", "avg(steal)", "avg(steal)"},
	{"steal_max", "max(steal)", "max(steal_max)"},
	{"softirq", "avg(softirq)", "avg(softirq)"},
	{"softirq_max", "max(softirq)", "max(softirq_max)"},
	{"load1", "avg(load1)", "avg(load1)"},
	{"load1_max", "max(load1)", "max(load1_max)"},
	{"load5", "avg(load5)", "avg(load5)"},
	{"load15", "avg(load15)", "avg(load15)"},
	{"mem_total", "max(mem_total)", "max(mem_total)"},
	{"mem_used", "avg(mem_used)", "avg(mem_used)"},
	{"mem_used_max", "max(mem_used)", "max(mem_used_max)"},
	{"swap_total", "max(swap_total)", "max(swap_total)"},
	{"swap_used", "avg(swap_used)", "avg(swap_used)"},
	{"swap_used_max", "max(swap_used)", "max(swap_used_max)"},
	{"tcp", "avg(tcp)", "avg(tcp)"},
	{"tcp_max", "max(tcp)", "max(tcp_max)"},
	{"udp", "avg(udp)", "avg(udp)"},
	{"udp_max", "max(udp)", "max(udp_max)"},
	{"tcp_tw", "avg(tcp_tw)", "avg(tcp_tw)"},
	{"threads", "avg(threads)", "avg(threads)"},
	{"threads_max", "max(threads)", "max(threads_max)"},
}

var netCols = []column{
	{"rx", "avg(rx)", "avg(rx)"},
	{"rx_max", "max(rx)", "max(rx_max)"},
	{"tx", "avg(tx)", "avg(tx)"},
	{"tx_max", "max(tx)", "max(tx_max)"},
	{"rx_pps", "avg(rx_pps)", "avg(rx_pps)"},
	{"rx_pps_max", "max(rx_pps)", "max(rx_pps_max)"},
	{"tx_pps", "avg(tx_pps)", "avg(tx_pps)"},
	{"tx_pps_max", "max(tx_pps)", "max(tx_pps_max)"},
}

var diskCols = []column{
	{"total", "max(total)", "max(total)"},
	{"used", "avg(used)", "avg(used)"},
	{"used_max", "max(used)", "max(used_max)"},
	{"avail", "avg(avail)", "avg(avail)"},
	{"inode_pct", "avg(inode_pct)", "avg(inode_pct)"},
}

// Same formulas work on raw and rollup rows, since rollups keep sums.
var pingCols = []column{
	{"sent", "sum(sent)", "sum(sent)"},
	{"lost", "sum(lost)", "sum(lost)"},
	{"loss_pct", "100.0 * sum(lost) / NULLIF(sum(sent), 0)", "100.0 * sum(lost) / NULLIF(sum(sent), 0)"},
	{"min", "min(min)", "min(min)"},
	{"avg", "sum(avg * (sent - lost)) / NULLIF(sum(CASE WHEN avg IS NULL THEN 0 ELSE sent - lost END), 0)",
		"sum(avg * (sent - lost)) / NULLIF(sum(CASE WHEN avg IS NULL THEN 0 ELSE sent - lost END), 0)"},
	{"max", "max(max)", "max(max)"},
	{"jitter", "avg(jitter)", "avg(jitter)"},
}

type tier struct {
	name   string
	suffix string
	step   int64
}

var (
	tierRaw = tier{"raw", "_raw", 10}
	tier5m  = tier{"5m", "_5m", step5m}
	tier1h  = tier{"1h", "_1h", step1h}
)

// pickTier chooses the finest resolution that covers [from, to): short
// ranges use raw data, but only while it is still retained.
func (s *Store) pickTier(from, to int64, has5m bool) (tier, int64) {
	now := s.now()
	span := to - from
	t := tier1h
	switch {
	case span <= int64(6*time.Hour/time.Second) && from >= now.Add(-s.ret.Raw).Unix():
		t = tierRaw
	case has5m && span <= int64(7*24*time.Hour/time.Second) && from >= now.Add(-s.ret.M5).Unix():
		t = tier5m
	case !has5m && span <= int64(2*24*time.Hour/time.Second) && from >= now.Add(-s.ret.Raw).Unix():
		t = tierRaw // disks: raw (60s) or 1h
	}
	step := t.step
	if need := (span + MaxPoints - 1) / MaxPoints; need > step {
		step = (need + t.step - 1) / t.step * t.step
	}
	return t, step
}

// Metrics returns CPU, load, memory and swap for a node.
func (s *Store) Metrics(ctx context.Context, node string, from, to int64) (*Series, error) {
	rid, ok := s.nodeID(node)
	if !ok {
		return nil, nil
	}
	t, step := s.pickTier(from, to, true)
	m, err := s.series(ctx, "metrics"+t.suffix, "node = ?", []any{rid}, "", metricCols, t, step, from, to)
	if err != nil {
		return nil, err
	}
	return m[""], nil
}

// Net returns per-interface rates, keyed by interface.
func (s *Store) Net(ctx context.Context, node string, from, to int64) (map[string]*Series, error) {
	rid, ok := s.nodeID(node)
	if !ok {
		return nil, nil
	}
	t, step := s.pickTier(from, to, true)
	return s.series(ctx, "net"+t.suffix, "node = ?", []any{rid}, "iface", netCols, t, step, from, to)
}

// Disks returns usage per mount point.
func (s *Store) Disks(ctx context.Context, node string, from, to int64) (map[string]*Series, error) {
	rid, ok := s.nodeID(node)
	if !ok {
		return nil, nil
	}
	t, step := s.pickTier(from, to, false)
	return s.series(ctx, "disk"+t.suffix, "node = ?", []any{rid}, "mount", diskCols, t, step, from, to)
}

// Ping returns the history of one link. dst is the peer name the source
// agent was configured with.
func (s *Store) Ping(ctx context.Context, src, dst string, from, to int64) (*Series, error) {
	rid, ok := s.nodeID(src)
	if !ok {
		return nil, nil
	}
	t, step := s.pickTier(from, to, true)
	m, err := s.series(ctx, "ping"+t.suffix, "src = ? AND dst = ?", []any{rid, dst}, "", pingCols, t, step, from, to)
	if err != nil {
		return nil, err
	}
	return m[""], nil
}

// series runs one bucketed query. key, if set, splits rows into separate
// series (per interface or mount).
func (s *Store) series(ctx context.Context, table, where string, args []any, key string,
	cols []column, t tier, step, from, to int64) (map[string]*Series, error) {
	exprs := make([]string, len(cols))
	for i, c := range cols {
		if t == tierRaw {
			exprs[i] = c.raw
		} else {
			exprs[i] = c.rollup
		}
	}
	keyExpr, keyGroup := "''", ""
	if key != "" {
		keyExpr, keyGroup = key, key+", "
	}
	q := fmt.Sprintf(`SELECT %s, ts / %d * %d AS b, %s FROM %s
		WHERE %s AND ts >= ? AND ts < ? GROUP BY %sb ORDER BY %sb`,
		keyExpr, step, step, strings.Join(exprs, ", "), table, where, keyGroup, keyGroup)
	args = append(args, from, to)

	out := map[string]*Series{}
	vals := make([]sql.NullFloat64, len(cols))
	dest := make([]any, 0, len(cols)+2)
	var k string
	var b int64
	dest = append(dest, &k, &b)
	for i := range vals {
		dest = append(dest, &vals[i])
	}
	err := s.each(ctx, q, args, func(r *sql.Rows) error {
		if err := r.Scan(dest...); err != nil {
			return err
		}
		sr := out[k]
		if sr == nil {
			sr = &Series{Tier: t.name, Step: step, TS: []int64{}, Cols: map[string][]*float64{}}
			for _, c := range cols {
				sr.Cols[c.name] = []*float64{}
			}
			out[k] = sr
		}
		sr.TS = append(sr.TS, b)
		for i, c := range cols {
			var p *float64
			if vals[i].Valid {
				v := vals[i].Float64
				p = &v
			}
			sr.Cols[c.name] = append(sr.Cols[c.name], p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if key == "" && out[""] == nil {
		out[""] = &Series{Tier: t.name, Step: step, TS: []int64{}, Cols: map[string][]*float64{}}
		for _, c := range cols {
			out[""].Cols[c.name] = []*float64{}
		}
	}
	return out, nil
}

// Link is one cell of the latency matrix.
type Link struct {
	Src     string   `json:"src"`
	Dst     string   `json:"dst"`
	Sent    int64    `json:"sent"`
	Lost    int64    `json:"lost"`
	LossPct float64  `json:"loss_pct"`
	Min     *float64 `json:"min"`
	Avg     *float64 `json:"avg"`
	Max     *float64 `json:"max"`
	Jitter  *float64 `json:"jitter"`
}

// Matrix summarizes every link over the last window, based on server time.
// Data from agents whose clocks are off by more than the window is missed;
// Status.Skew shows that.
func (s *Store) Matrix(ctx context.Context, window time.Duration) ([]Link, error) {
	now := s.now()
	out := []Link{}
	err := s.each(ctx, `SELECT src, dst, sum(sent), sum(lost), min(min),
			sum(avg * (sent - lost)) / NULLIF(sum(CASE WHEN avg IS NULL THEN 0 ELSE sent - lost END), 0),
			max(max), avg(jitter)
		FROM ping_raw WHERE ts >= ? AND ts <= ? GROUP BY src, dst ORDER BY src, dst`,
		[]any{now.Add(-window).Unix(), now.Unix() + 60}, func(r *sql.Rows) error {
			var rid int64
			var l Link
			if err := r.Scan(&rid, &l.Dst, &l.Sent, &l.Lost, &l.Min, &l.Avg, &l.Max, &l.Jitter); err != nil {
				return err
			}
			name, ok := s.nodeName(rid)
			if !ok {
				return nil // node removed from config
			}
			l.Src = name
			if l.Sent > 0 {
				l.LossPct = 100 * float64(l.Lost) / float64(l.Sent)
			}
			out = append(out, l)
			return nil
		})
	return out, err
}

// NetSum is one sample's rates summed over the node's interfaces.
type NetSum struct {
	TS int64
	RX float64 // bytes/s
	TX float64
	// Packets/s; invalid when no interface reported them (agent < 0.1.14).
	RXPkts, TXPkts sql.NullFloat64
}

// NetSums lists the node's samples with from <= ts <= to (agent clock),
// oldest first.
func (s *Store) NetSums(ctx context.Context, node string, from, to int64) ([]NetSum, error) {
	rid, ok := s.nodeID(node)
	if !ok {
		return nil, nil
	}
	var out []NetSum
	err := s.each(ctx, `SELECT ts, sum(rx), sum(tx), sum(rx_pps), sum(tx_pps) FROM net_raw
		WHERE node = ? AND ts >= ? AND ts <= ? GROUP BY ts ORDER BY ts`, []any{rid, from, to}, func(r *sql.Rows) error {
		var n NetSum
		if err := r.Scan(&n.TS, &n.RX, &n.TX, &n.RXPkts, &n.TXPkts); err != nil {
			return err
		}
		out = append(out, n)
		return nil
	})
	return out, err
}

func (s *Store) each(ctx context.Context, q string, args []any, fn func(*sql.Rows) error) error {
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// DownLossPct marks a 5-minute period as unavailable: at least this share
// of its pings got no reply. It matches the default ping_loss rule.
const DownLossPct = 20

// Availability is one link's history in equal cells, from the 5m rollups.
// Periods counts 5-minute periods with data, Down those at or above
// DownLossPct; a cell without data has zero periods.
type Availability struct {
	Src      string   `json:"src"`
	Dst      string   `json:"dst"`
	Periods  int64    `json:"periods"`
	Down     int64    `json:"down"`
	AvailPct *float64 `json:"avail_pct"` // null without data
	Sent     int64    `json:"sent"`
	Lost     int64    `json:"lost"`
	Avg      *float64 `json:"avg"`
	Cells    Cells    `json:"cells"`
}

// Cells is column-oriented: index i covers [from + i*cell, from + (i+1)*cell).
type Cells struct {
	Periods []int64    `json:"periods"`
	Down    []int64    `json:"down"`
	Sent    []int64    `json:"sent"`
	Lost    []int64    `json:"lost"`
	Avg     []*float64 `json:"avg"`
}

// Availability splits [from, from + n*cell) into n cells for every link
// with data in that span. cell must be a multiple of 5 minutes.
func (s *Store) Availability(ctx context.Context, from, cell int64, n int) ([]*Availability, error) {
	out := []*Availability{}
	byLink := map[[2]string]*Availability{}
	avgW := map[*Availability]float64{} // sum(avg * replies) over the whole span
	replies := map[*Availability]int64{}
	err := s.each(ctx, `SELECT src, dst, (ts - ?1) / ?2 AS i, count(*), sum(lost * 100 >= sent * ?3),
			sum(sent), sum(lost),
			sum(avg * (sent - lost)), sum(CASE WHEN avg IS NULL THEN 0 ELSE sent - lost END)
		FROM ping_5m WHERE ts >= ?1 AND ts < ?1 + ?2 * ?4 AND sent > 0
		GROUP BY src, dst, i ORDER BY src, dst, i`,
		[]any{from, cell, DownLossPct, n}, func(r *sql.Rows) error {
			var rid, i, periods, down, sent, lost, rep int64
			var dst string
			var w sql.NullFloat64
			if err := r.Scan(&rid, &dst, &i, &periods, &down, &sent, &lost, &w, &rep); err != nil {
				return err
			}
			src, ok := s.nodeName(rid)
			if !ok || i < 0 || i >= int64(n) {
				return nil
			}
			a := byLink[[2]string{src, dst}]
			if a == nil {
				a = &Availability{Src: src, Dst: dst, Cells: Cells{Periods: make([]int64, n), Down: make([]int64, n),
					Sent: make([]int64, n), Lost: make([]int64, n), Avg: make([]*float64, n)}}
				byLink[[2]string{src, dst}] = a
				out = append(out, a)
			}
			c := &a.Cells
			c.Periods[i], c.Down[i], c.Sent[i], c.Lost[i] = periods, down, sent, lost
			if rep > 0 && w.Valid {
				v := w.Float64 / float64(rep)
				c.Avg[i] = &v
				avgW[a] += w.Float64
				replies[a] += rep
			}
			a.Periods += periods
			a.Down += down
			a.Sent += sent
			a.Lost += lost
			return nil
		})
	for _, a := range out {
		if a.Periods > 0 {
			v := 100 * float64(a.Periods-a.Down) / float64(a.Periods)
			a.AvailPct = &v
		}
		if replies[a] > 0 {
			v := avgW[a] / float64(replies[a])
			a.Avg = &v
		}
	}
	return out, err
}

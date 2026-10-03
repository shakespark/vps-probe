package store

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "vpsprobe/internal/proto/probev1"
)

var (
	ctx     = context.Background()
	discard = slog.New(slog.NewTextHandler(io.Discard, nil))
	sh, _   = time.LoadLocation("Asia/Shanghai")
	ret     = Retention{Raw: 48 * time.Hour, M5: 30 * 24 * time.Hour, H1: 400 * 24 * time.Hour}
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, Options{Location: sh, Retention: ret, Log: discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncNodes([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func newStore(t *testing.T) *Store {
	s := open(t, filepath.Join(t.TempDir(), "probe.db"))
	t.Cleanup(func() { s.Close() })
	return s
}

var testIP = netip.MustParseAddr("192.0.2.1")

func write(t *testing.T, s *Store, node string, rep *pb.Report, arrival time.Time) {
	t.Helper()
	if err := s.Write(node, rep, testIP, arrival); err != nil {
		t.Fatal(err)
	}
}

func TestSplitPiecesMerge(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	ts := now.Unix() / 10 * 10
	write(t, s, "a", &pb.Report{Ts: ts, Cpu: &pb.CPU{Usage: 12.5, Steal: 1}}, now)
	write(t, s, "a", &pb.Report{Ts: ts, Mem: &pb.Mem{Total: 1000, Used: 400}}, now)
	// A duplicate of the first piece must not wipe the memory columns.
	write(t, s, "a", &pb.Report{Ts: ts, Cpu: &pb.CPU{Usage: 12.5, Steal: 1}}, now)

	st, err := s.Status(ctx, "a")
	if err != nil || st == nil {
		t.Fatal(st, err)
	}
	if st.CPU == nil || *st.CPU != 12.5 || st.MemUsed == nil || *st.MemUsed != 400 || st.Load1 != nil {
		t.Fatalf("merged row: cpu=%v mem=%v load=%v", st.CPU, st.MemUsed, st.Load1)
	}
	var rows int
	s.r.QueryRow("SELECT count(*) FROM metrics_raw").Scan(&rows)
	if rows != 1 {
		t.Fatalf("rows = %d", rows)
	}
}

func traffic(ts int64, cur string, rx, tx uint64, prev string, prx, ptx uint64) *pb.Report {
	return &pb.Report{Ts: ts, Traffic: []*pb.IfaceTraffic{{
		Iface: "eth0",
		Cur:   &pb.Period{Start: cur, Rx: rx, Tx: tx},
		Prev:  &pb.Period{Start: prev, Rx: prx, Tx: ptx},
	}}}
}

func TestTrafficLatestTSWins(t *testing.T) {
	s := newStore(t)
	base := time.Date(2026, 9, 10, 12, 0, 0, 0, sh).Unix()
	now := time.Unix(base, 0)
	cur := func() Period {
		p, err := s.Periods(ctx, "a", 1)
		if err != nil || len(p) != 1 {
			t.Fatal(p, err)
		}
		return p[0]
	}

	write(t, s, "a", traffic(base, "2026-09-01", 1000, 100, "2026-08-01", 5000, 500), now)
	// Older report (backlog or replay) arrives later: ignored.
	write(t, s, "a", traffic(base-10, "2026-09-01", 900, 90, "2026-08-01", 5000, 500), now)
	if p := cur(); p.RX != 1000 || p.TX != 100 {
		t.Fatalf("after older report: %+v", p)
	}
	// Agent state rebuilt: totals legitimately drop, and prev is empty.
	write(t, s, "a", traffic(base+10, "2026-09-01", 20, 2, "2026-08-01", 0, 0), now)
	if p := cur(); p.RX != 20 || p.TX != 2 {
		t.Fatalf("after reset: %+v", p)
	}
	all, _ := s.Periods(ctx, "a", 24)
	if len(all) != 2 || all[1].Start != "2026-08-01" || all[1].RX != 5000 {
		t.Fatalf("previous period lost: %+v", all)
	}
}

func TestDaily(t *testing.T) {
	s := newStore(t)
	day := func(d, h int) int64 { return time.Date(2026, 9, d, h, 0, 0, 0, sh).Unix() }
	for _, r := range []struct {
		ts     int64
		rx, tx uint64
	}{
		{day(1, 1), 10, 1},
		{day(1, 23), 100, 10}, // day 1 ends at 100
		{day(2, 12), 250, 25}, // day 2: +150
		{day(3, 8), 300, 30},
		{day(3, 20), 40, 4}, // state rebuilt on day 3: counts its own total
		{day(4, 0), 90, 9},  // day 4: +50
	} {
		write(t, s, "a", traffic(r.ts, "2026-09-01", r.rx, r.tx, "2026-08-01", 0, 0), time.Unix(r.ts, 0))
	}
	// A second interface adds to the same days.
	write(t, s, "a", &pb.Report{Ts: day(2, 5), Traffic: []*pb.IfaceTraffic{{
		Iface: "eth1", Cur: &pb.Period{Start: "2026-09-01", Rx: 7, Tx: 7}}}}, time.Now())

	days, err := s.Daily(ctx, "a", "2026-09-01")
	if err != nil {
		t.Fatal(err)
	}
	want := []DayTraffic{{"2026-09-01", 100, 10}, {"2026-09-02", 157, 22}, {"2026-09-03", 40, 4}, {"2026-09-04", 50, 5}}
	if len(days) != len(want) {
		t.Fatalf("got %+v", days)
	}
	for i := range want {
		if days[i] != want[i] {
			t.Fatalf("day %d: got %+v want %+v", i, days[i], want[i])
		}
	}
}

func TestFreshnessIgnoresOldReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.db")
	s := open(t, path)
	t0 := time.Now().Truncate(time.Second)
	write(t, s, "a", &pb.Report{Ts: t0.Unix()}, t0)
	// A replayed older packet arriving a minute later must not refresh it.
	write(t, s, "a", &pb.Report{Ts: t0.Unix() - 30}, t0.Add(time.Minute))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Survives a restart.
	s = open(t, path)
	defer s.Close()
	write(t, s, "a", &pb.Report{Ts: t0.Unix() - 20}, t0.Add(2*time.Minute))
	st, _ := s.Status(ctx, "a")
	if st.FreshAt != t0.Unix() || st.MaxTS != t0.Unix() {
		t.Fatalf("fresh_at=%d max_ts=%d, want %d", st.FreshAt, st.MaxTS, t0.Unix())
	}
	write(t, s, "a", &pb.Report{Ts: t0.Unix() + 10}, t0.Add(3*time.Minute))
	st, _ = s.Status(ctx, "a")
	if st.FreshAt != t0.Add(3*time.Minute).Unix() {
		t.Fatalf("newer report did not refresh: %d", st.FreshAt)
	}
}

func TestSysKeepsNewest(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	write(t, s, "a", &pb.Report{Ts: 200, Sys: &pb.SysInfo{Hostname: "new"}}, now)
	write(t, s, "a", &pb.Report{Ts: 100, Sys: &pb.SysInfo{Hostname: "old"}}, now)
	st, _ := s.Status(ctx, "a")
	if st.Sys == nil || st.Sys.Hostname != "new" {
		t.Fatalf("sys = %+v", st.Sys)
	}
}

func TestRollupAndTiers(t *testing.T) {
	s := newStore(t)
	now := time.Now().Truncate(time.Hour).Add(30 * time.Minute)
	s.now = func() time.Time { return now }
	start := now.Add(-2 * time.Hour).Unix()
	for ts := start; ts < now.Unix(); ts += 10 {
		cpu := float32(10)
		if (ts/10)%2 == 1 {
			cpu = 30
		}
		write(t, s, "a", &pb.Report{Ts: ts, Cpu: &pb.CPU{Usage: cpu},
			Net:   []*pb.NetRate{{Iface: "eth0", RxRate: 100, TxRate: 50}},
			Pings: []*pb.Ping{{Target: "b", Sent: 10, Lost: 2, Min: 5, Avg: 10, Max: 20}, {Target: "void", Sent: 10, Lost: 10}}},
			now)
	}
	if err := s.Rollup(now.Add(-3 * time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 1h range from raw: 360 samples fit in MaxPoints without regrouping.
	m, err := s.Metrics(ctx, "a", now.Add(-time.Hour).Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if m.Tier != "raw" || m.Step != 10 || len(m.TS) != 360 {
		t.Fatalf("raw: tier=%s step=%d n=%d", m.Tier, m.Step, len(m.TS))
	}
	// 3 days: 5m tier, 864 buckets max, only the last 2h have data.
	m, err = s.Metrics(ctx, "a", now.Add(-72*time.Hour).Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if m.Tier != "5m" || m.Step != 300 || len(m.TS) != 24 {
		t.Fatalf("5m: tier=%s step=%d n=%d", m.Tier, m.Step, len(m.TS))
	}
	if avg, mx := *m.Cols["cpu"][0], *m.Cols["cpu_max"][0]; avg != 20 || mx != 30 {
		t.Fatalf("5m cpu avg=%v max=%v", avg, mx)
	}
	// 30 days: 1h tier.
	m, _ = s.Metrics(ctx, "a", now.Add(-30*24*time.Hour).Unix(), now.Unix())
	if m.Tier != "1h" || len(m.TS) != 3 {
		t.Fatalf("1h: tier=%s n=%d", m.Tier, len(m.TS))
	}

	p, err := s.Ping(ctx, "a", "b", now.Add(-72*time.Hour).Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	if *p.Cols["loss_pct"][0] != 20 || *p.Cols["avg"][0] != 10 || *p.Cols["min"][0] != 5 {
		t.Fatalf("ping rollup: loss=%v avg=%v min=%v", *p.Cols["loss_pct"][0], *p.Cols["avg"][0], *p.Cols["min"][0])
	}
	v, _ := s.Ping(ctx, "a", "void", now.Add(-time.Hour).Unix(), now.Unix())
	if *v.Cols["loss_pct"][0] != 100 || v.Cols["avg"][0] != nil {
		t.Fatalf("all-lost link: loss=%v avg=%v", *v.Cols["loss_pct"][0], v.Cols["avg"][0])
	}

	n, err := s.Net(ctx, "a", now.Add(-time.Hour).Unix(), now.Unix())
	if err != nil || n["eth0"] == nil || *n["eth0"].Cols["rx"][0] != 100 {
		t.Fatalf("net: %v %v", n, err)
	}

	links, err := s.Matrix(ctx, 5*time.Minute)
	if err != nil || len(links) != 2 {
		t.Fatalf("matrix: %+v %v", links, err)
	}
	if links[0].Dst != "b" || links[0].LossPct != 20 || links[1].Dst != "void" || links[1].Avg != nil {
		t.Fatalf("matrix: %+v", links)
	}

	// Retention: raw older than the window is deleted, rollups stay.
	s.now = func() time.Time { return now.Add(49 * time.Hour) }
	if err := s.Cleanup(ret); err != nil {
		t.Fatal(err)
	}
	var raw, r5 int
	s.r.QueryRow("SELECT count(*) FROM metrics_raw").Scan(&raw)
	s.r.QueryRow("SELECT count(*) FROM metrics_5m").Scan(&r5)
	if raw != 0 || r5 != 24 {
		t.Fatalf("after cleanup raw=%d 5m=%d", raw, r5)
	}
}

func TestRollupDoesNotShrinkTrimmedBucket(t *testing.T) {
	s := newStore(t)
	h := time.Now().Truncate(time.Hour).Add(-5 * time.Hour)
	half := h.Add(30 * time.Minute).Unix()
	for ts := h.Unix(); ts < h.Add(time.Hour).Unix(); ts += 10 {
		cpu := float32(50)
		if ts < half {
			cpu = 10
		}
		write(t, s, "a", &pb.Report{Ts: ts, Cpu: &pb.CPU{Usage: cpu}}, time.Now())
	}
	if err := s.Rollup(h); err != nil {
		t.Fatal(err)
	}
	// Retention removes the first half of the hour, then a rollup starting
	// mid-bucket runs (as at startup).
	s.w.Exec("DELETE FROM metrics_raw WHERE ts < ?", half)
	if err := s.Rollup(time.Unix(half, 0)); err != nil {
		t.Fatal(err)
	}
	var cpu float64
	s.r.QueryRow("SELECT cpu FROM metrics_1h WHERE ts = ?", h.Unix()).Scan(&cpu)
	if cpu != 30 {
		t.Fatalf("1h bucket cpu = %v, want 30 (recomputed from half the data?)", cpu)
	}
	var cnt5 int
	s.r.QueryRow("SELECT count(*) FROM metrics_5m WHERE ts >= ? AND ts < ?", h.Unix(), h.Add(time.Hour).Unix()).Scan(&cnt5)
	if cnt5 != 12 {
		t.Fatalf("5m buckets = %d, want 12", cnt5)
	}
}

func TestBackupAndSingleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.db")
	s := open(t, path)
	write(t, s, "a", traffic(time.Now().Unix(), "2026-09-01", 123, 45, "2026-08-01", 0, 0), time.Now())

	// Live backup while the store is open.
	out := filepath.Join(dir, "copy.db")
	if err := Backup(ctx, path, out); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, path, out); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", fi.Mode())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + sfx); err == nil {
			t.Fatalf("%s left behind after Close", sfx)
		}
	}

	c := open(t, out)
	defer c.Close()
	p, err := c.Periods(ctx, "a", 1)
	if err != nil || len(p) != 1 || p[0].RX != 123 {
		t.Fatalf("backup content: %+v %v", p, err)
	}
}

func TestDailyBackupRotation(t *testing.T) {
	s := newStore(t)
	dir := filepath.Join(t.TempDir(), "backups")
	day := time.Date(2026, 9, 1, 5, 0, 0, 0, sh)
	for i := 0; i < 5; i++ {
		d := day.AddDate(0, 0, i)
		s.now = func() time.Time { return d }
		made, err := s.DailyBackup(ctx, dir, 3)
		if err != nil || made == "" {
			t.Fatal(made, err)
		}
		// Second call the same day is a no-op.
		if again, _ := s.DailyBackup(ctx, dir, 3); again != "" {
			t.Fatal("backed up twice in one day")
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 || entries[0].Name() != "probe-20260903.db" {
		t.Fatalf("kept %v", entries)
	}
}

func TestUnknownNodeRejected(t *testing.T) {
	s := newStore(t)
	if err := s.Write("zzz", &pb.Report{Ts: 1}, testIP, time.Now()); err == nil {
		t.Fatal("write for unknown node succeeded")
	}
	if st, err := s.Status(ctx, "b"); st != nil || err != nil {
		t.Fatalf("never-reported node: %v %v", st, err)
	}
}

// The source IP follows the newest report only: a replayed older packet
// from another address leaves it alone.
func TestSourceIP(t *testing.T) {
	s := newStore(t)
	t0 := time.Now().Unix()
	at := func(ts int64, ip string) {
		t.Helper()
		if err := s.Write("a", &pb.Report{Ts: ts}, netip.MustParseAddr(ip), time.Unix(ts, 0)); err != nil {
			t.Fatal(err)
		}
	}
	check := func(ip string, since int64) {
		t.Helper()
		st, err := s.Status(ctx, "a")
		if err != nil || st.IP != ip || st.IPSince != since {
			t.Fatalf("ip = %q since %d, want %q since %d (%v)", st.IP, st.IPSince, ip, since, err)
		}
	}
	at(t0, "::ffff:198.51.100.1") // IPv4 through a dual-stack socket
	check("198.51.100.1", t0)
	at(t0+10, "198.51.100.1")
	check("198.51.100.1", t0)
	at(t0-10, "203.0.113.9") // replay of an older report
	at(t0+10, "203.0.113.9") // same ts as the newest: not newer
	check("198.51.100.1", t0)
	at(t0+20, "2001:db8::1")
	check("2001:db8::1", t0+20)
}

func TestAvailability(t *testing.T) {
	s := newStore(t)
	now := time.Now().Truncate(time.Hour).Add(-2 * time.Hour)
	start := now.Add(-time.Hour).Unix()
	bad := now.Add(-30 * time.Minute).Unix() // one 5m period at 50% loss
	for ts := start; ts < now.Unix(); ts += 10 {
		p := &pb.Ping{Target: "b", Sent: 10, Lost: 0, Min: 5, Avg: 10, Max: 20}
		if ts >= bad && ts < bad+300 {
			p.Lost, p.Avg = 5, 20
		}
		write(t, s, "a", &pb.Report{Ts: ts, Pings: []*pb.Ping{p, {Target: "void", Sent: 10, Lost: 10}}}, time.Unix(ts, 0))
	}
	if err := s.Rollup(time.Unix(start, 0)); err != nil {
		t.Fatal(err)
	}
	links, err := s.Availability(ctx, start, 1800, 3) // the third cell has no data
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 2 || links[0].Dst != "b" || links[1].Dst != "void" {
		t.Fatalf("links: %+v", links)
	}
	b := links[0]
	c := b.Cells
	if b.Periods != 12 || b.Down != 1 || c.Periods[0] != 6 || c.Down[0] != 0 || c.Down[1] != 1 || c.Periods[2] != 0 {
		t.Fatalf("periods: %+v %+v", b, c)
	}
	if *b.AvailPct != 100*11.0/12 || b.Sent != 3600 || b.Lost != 150 {
		t.Fatalf("totals: avail %v sent %d lost %d", *b.AvailPct, b.Sent, b.Lost)
	}
	// Cell 1: 150 replies at 20 ms and 1500 at 10 ms.
	if got := *c.Avg[1]; got < 10.9 || got > 10.91 || *c.Avg[0] != 10 || c.Avg[2] != nil {
		t.Fatalf("cell avg: %v %v %v", *c.Avg[0], got, c.Avg[2])
	}
	v := links[1]
	if *v.AvailPct != 0 || v.Down != 12 || v.Avg != nil || v.Cells.Avg[0] != nil {
		t.Fatalf("dead link: %+v", v)
	}
}

// Sockets and threads (agent >= 0.1.9) merge into the metrics row, roll up,
// and are null for older agents.
func TestSocketsAndThreads(t *testing.T) {
	s := newStore(t)
	now := time.Now().Truncate(time.Hour).Add(-2 * time.Hour)
	for i, ts := 0, now.Unix(); ts < now.Add(5*time.Minute).Unix(); i, ts = i+1, ts+10 {
		// Split pieces: CPU in one, the rest in another.
		write(t, s, "a", &pb.Report{Ts: ts, Cpu: &pb.CPU{Usage: 1}}, time.Unix(ts, 0))
		write(t, s, "a", &pb.Report{Ts: ts, Load: &pb.Load{L1: 1, Threads: uint32(300 + i)},
			Sockets: &pb.Sockets{Tcp: uint32(40 + i%2*20), Udp: 7, TcpTw: 3}}, time.Unix(ts, 0))
		write(t, s, "b", &pb.Report{Ts: ts, Cpu: &pb.CPU{Usage: 1}, Load: &pb.Load{L1: 1}}, time.Unix(ts, 0)) // old agent
	}
	st, err := s.Status(ctx, "a")
	if err != nil || st.TCP == nil || *st.TCP != 60 || *st.UDP != 7 || *st.TCPTW != 3 || *st.Threads != 329 || st.CPU == nil {
		t.Fatalf("status a: %+v %v", st, err)
	}
	if st, _ := s.Status(ctx, "b"); st.TCP != nil || st.Threads != nil {
		t.Fatalf("old agent: tcp %v threads %v", st.TCP, st.Threads)
	}
	if err := s.Rollup(now); err != nil {
		t.Fatal(err)
	}
	m, err := s.Metrics(ctx, "a", now.Unix(), now.Add(7*24*time.Hour).Unix()) // 5m tier
	if err != nil || m.Tier != "5m" || len(m.TS) != 1 {
		t.Fatalf("5m: %+v %v", m, err)
	}
	if *m.Cols["tcp"][0] != 50 || *m.Cols["tcp_max"][0] != 60 || *m.Cols["threads_max"][0] != 329 || *m.Cols["tcp_tw"][0] != 3 {
		t.Fatalf("rollup: tcp %v max %v threads_max %v", *m.Cols["tcp"][0], *m.Cols["tcp_max"][0], *m.Cols["threads_max"][0])
	}
}

func TestNetSums(t *testing.T) {
	s := newStore(t)
	now := time.Now().Truncate(time.Minute)
	for i, ts := int64(0), now.Unix(); i < 4; i, ts = i+1, ts+10 {
		write(t, s, "a", &pb.Report{Ts: ts, Net: []*pb.NetRate{{Iface: "eth0", RxRate: 100, TxRate: 10},
			{Iface: "eth1", RxRate: uint64(i), TxRate: 1}}}, time.Unix(ts, 0))
	}
	got, err := s.NetSums(ctx, "a", now.Unix()+10, now.Unix()+20)
	if err != nil || len(got) != 2 || got[0] != (NetSum{TS: now.Unix() + 10, RX: 101, TX: 11}) || got[1].RX != 102 {
		t.Fatalf("sums: %+v %v", got, err)
	}
	if got, _ := s.NetSums(ctx, "zz", 0, now.Unix()+100); got != nil {
		t.Fatalf("unknown node: %+v", got)
	}
}

func TestPacketRatesAndSoftIRQ(t *testing.T) {
	s := newStore(t)
	now := time.Now().Truncate(time.Hour).Add(-2 * time.Hour)
	u := func(v uint64) *uint64 { return &v }
	f := func(v float32) *float32 { return &v }
	for i, ts := 0, now.Unix(); ts < now.Add(5*time.Minute).Unix(); i, ts = i+1, ts+10 {
		write(t, s, "a", &pb.Report{Ts: ts, Cpu: &pb.CPU{Usage: 20, Softirq: f(float32(2 + i%2*4))},
			Net: []*pb.NetRate{{Iface: "eth0", RxRate: 1000, TxRate: 500, RxPps: u(uint64(100 + i%2*200)), TxPps: u(50)},
				{Iface: "eth1", RxRate: 10, TxRate: 10, RxPps: u(1), TxPps: u(0)}}}, time.Unix(ts, 0))
		write(t, s, "b", &pb.Report{Ts: ts, Cpu: &pb.CPU{Usage: 1},
			Net: []*pb.NetRate{{Iface: "eth0", RxRate: 1, TxRate: 1}}}, time.Unix(ts, 0)) // old agent
	}
	st, err := s.Status(ctx, "a")
	if err != nil || st.SoftIRQ == nil || *st.SoftIRQ != 6 {
		t.Fatalf("status a: softirq %v %v", st.SoftIRQ, err)
	}
	if st, _ := s.Status(ctx, "b"); st.SoftIRQ != nil {
		t.Fatalf("old agent softirq %v", *st.SoftIRQ)
	}
	sums, err := s.NetSums(ctx, "a", now.Unix()+10, now.Unix()+10)
	if err != nil || len(sums) != 1 || !sums[0].RXPkts.Valid || sums[0].RXPkts.Float64 != 301 || sums[0].TXPkts.Float64 != 50 {
		t.Fatalf("sums a: %+v %v", sums, err)
	}
	if sums, _ := s.NetSums(ctx, "b", now.Unix(), now.Unix()); len(sums) != 1 || sums[0].RXPkts.Valid {
		t.Fatalf("sums b: %+v", sums)
	}
	if err := s.Rollup(now); err != nil {
		t.Fatal(err)
	}
	m, err := s.Metrics(ctx, "a", now.Unix(), now.Add(7*24*time.Hour).Unix()) // 5m tier
	if err != nil || m.Tier != "5m" || *m.Cols["softirq"][0] != 4 || *m.Cols["softirq_max"][0] != 6 {
		t.Fatalf("metrics 5m: %+v %v", m, err)
	}
	n, err := s.Net(ctx, "a", now.Unix(), now.Add(7*24*time.Hour).Unix())
	if err != nil || *n["eth0"].Cols["rx_pps"][0] != 200 || *n["eth0"].Cols["rx_pps_max"][0] != 300 {
		t.Fatalf("net 5m: %v", err)
	}
	if n, _ := s.Net(ctx, "b", now.Unix(), now.Add(7*24*time.Hour).Unix()); n["eth0"].Cols["rx_pps"][0] != nil {
		t.Fatal("old agent rolled up a packet rate")
	}
}

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

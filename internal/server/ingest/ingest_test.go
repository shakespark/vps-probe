package ingest

import (
	"context"
	"crypto/cipher"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"vpsprobe/internal/agent/report"
	pb "vpsprobe/internal/proto/probev1"
	"vpsprobe/internal/server/config"
	"vpsprobe/internal/server/store"
	"vpsprobe/internal/wire"
)

const (
	node  = "hk-1"
	token = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeWriter struct {
	mu   sync.Mutex
	reps []*pb.Report
	from []netip.Addr
	fail bool
}

func (f *fakeWriter) Write(node string, rep *pb.Report, from netip.Addr, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return fmt.Errorf("disk full")
	}
	f.reps = append(f.reps, rep)
	f.from = append(f.from, from)
	return nil
}

func (f *fakeWriter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reps)
}

func start(t *testing.T, w Writer) *Server {
	t.Helper()
	s, err := Listen("127.0.0.1:0", []config.Node{{ID: node, Token: token}}, w, discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return s
}

func dial(t *testing.T, s *Server) *net.UDPConn {
	t.Helper()
	c, err := net.DialUDP("udp", nil, s.Addr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func aead(t *testing.T, tok string) cipher.AEAD {
	t.Helper()
	a, err := wire.NewAEAD(tok, node)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func sealReport(t *testing.T, a cipher.AEAD, typ byte, rep *pb.Report) []byte {
	t.Helper()
	body, err := proto.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := wire.Seal(a, typ, node, body)
	if err != nil {
		t.Fatal(err)
	}
	return pkt
}

// exchange sends pkt and returns the reply, or nil if none came.
func exchange(t *testing.T, c *net.UDPConn, pkt []byte) []byte {
	t.Helper()
	if _, err := c.Write(pkt); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, 2048)
	n, err := c.Read(buf)
	if err != nil {
		return nil
	}
	return buf[:n]
}

func ackIDs(t *testing.T, a cipher.AEAD, pkt []byte) []uint64 {
	t.Helper()
	h, err := wire.ParseHeader(pkt)
	if err != nil || h.Type != wire.TypeAck || h.Node != node {
		t.Fatalf("bad ack header: %+v %v", h, err)
	}
	pt, err := h.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	var ack pb.Ack
	if err := proto.Unmarshal(pt, &ack); err != nil {
		t.Fatal(err)
	}
	return ack.Ids
}

func TestAcceptAndAck(t *testing.T) {
	w := &fakeWriter{}
	s := start(t, w)
	c := dial(t, s)
	a := aead(t, token)

	rep := &pb.Report{Ts: time.Now().Unix(), Id: 42, Cpu: &pb.CPU{Usage: 5}}
	reply := exchange(t, c, sealReport(t, a, wire.TypeReport, rep))
	if reply == nil {
		t.Fatal("no ack")
	}
	if ids := ackIDs(t, a, reply); len(ids) != 1 || ids[0] != 42 {
		t.Fatalf("ack ids %v", ids)
	}
	if len(reply) >= len(sealReport(t, a, wire.TypeReport, rep)) {
		t.Fatal("ack not smaller than the report")
	}
	// A retransmission is acknowledged but not written again.
	if exchange(t, c, sealReport(t, a, wire.TypeReport, rep)) == nil {
		t.Fatal("duplicate not acked")
	}
	if w.count() != 1 {
		t.Fatalf("writes = %d", w.count())
	}
	if !w.from[0].IsLoopback() {
		t.Fatalf("source address %v", w.from[0])
	}
	st := s.Stats()
	if st[Accepted] != 1 || st[Duplicate] != 1 {
		t.Fatalf("stats %v", st)
	}
}

func TestSilentDrops(t *testing.T) {
	w := &fakeWriter{}
	s := start(t, w)
	c := dial(t, s)
	good := aead(t, token)
	now := time.Now().Unix()
	valid := sealReport(t, good, wire.TypeReport, &pb.Report{Ts: now, Id: 1})

	other, _ := wire.NewAEAD(token, "jp-1")
	unknown, _ := wire.Seal(other, wire.TypeReport, "jp-1", []byte{})
	junk, _ := wire.Seal(good, wire.TypeReport, node, []byte{0xff, 0xff, 0xff})
	tampered := append([]byte(nil), valid...)
	tampered[len(tampered)-1] ^= 1

	cases := []struct {
		name, counter string
		pkt           []byte
	}{
		{"empty", Malformed, []byte{}},
		{"garbage", Malformed, []byte("GET / HTTP/1.1\r\n\r\n")},
		{"oversized", Malformed, make([]byte, 1500)},
		{"unknown node", UnknownNode, unknown},
		{"wrong token", AuthFailed, sealReport(t, aead(t, token+"x"), wire.TypeReport, &pb.Report{Ts: now})},
		{"tampered", AuthFailed, tampered},
		{"ack reflected as report", WrongType, sealReport(t, good, wire.TypeAck, &pb.Report{Ts: now})},
		{"bad protobuf", DecodeFailed, junk},
		{"too old", TSOutOfRange, sealReport(t, good, wire.TypeReport, &pb.Report{Ts: now - 3*3600})},
		{"from the future", TSOutOfRange, sealReport(t, good, wire.TypeReport, &pb.Report{Ts: now + 3*3600})},
	}
	for _, tc := range cases {
		before := s.Stats()[tc.counter]
		if reply := exchange(t, c, tc.pkt); reply != nil {
			t.Errorf("%s: got a reply", tc.name)
		}
		if s.Stats()[tc.counter] != before+1 {
			t.Errorf("%s: counter %s not incremented: %v", tc.name, tc.counter, s.Stats())
		}
	}
	if w.count() != 0 {
		t.Fatalf("writes = %d", w.count())
	}
}

func TestStoreFailureNotAcked(t *testing.T) {
	w := &fakeWriter{fail: true}
	s := start(t, w)
	c := dial(t, s)
	a := aead(t, token)
	pkt := sealReport(t, a, wire.TypeReport, &pb.Report{Ts: time.Now().Unix(), Id: 7})
	if exchange(t, c, pkt) != nil {
		t.Fatal("acked a report that was not stored")
	}
	// The agent's retry must be stored once the store recovers, not
	// swallowed as a duplicate.
	w.mu.Lock()
	w.fail = false
	w.mu.Unlock()
	if exchange(t, c, pkt) == nil || w.count() != 1 {
		t.Fatalf("retry: writes = %d", w.count())
	}
}

func TestSanitize(t *testing.T) {
	nan := float32(math.NaN())
	rep := &pb.Report{
		Sys:  &pb.SysInfo{Hostname: "ok", Os: "bad\x1b[31m"},
		Cpu:  &pb.CPU{Usage: nan},
		Load: &pb.Load{L1: 1, L5: 1, L15: 1},
		Mem:  &pb.Mem{Total: 10, Used: 11},
		Disks: []*pb.Disk{
			{Mount: "/", Total: 10, Used: 5, Avail: 5, InodePct: 3},
			{Mount: "/x", Total: math.MaxUint64, Used: 1},
		},
		Net: []*pb.NetRate{{Iface: "", RxRate: 1}, {Iface: "eth0", RxRate: 1}},
		Traffic: []*pb.IfaceTraffic{
			{Iface: "eth0", Cur: &pb.Period{Start: "2026-09-01", Rx: 1}},
			{Iface: "eth1", Cur: &pb.Period{Start: "2026-13-01"}},
			{Iface: "eth2", Cur: &pb.Period{Start: "2026-09-01", Rx: math.MaxUint64}},
		},
		Pings: []*pb.Ping{
			{Target: "b", Sent: 10, Lost: 10, Avg: float32(math.Inf(1))}, // RTT ignored when all lost
			{Target: "c", Sent: 10, Lost: 11},
			{Target: "d", Sent: 10, Lost: 0, Min: 5, Avg: 4, Max: 6},
			{Target: "e", Sent: 10, Lost: 0, Min: 5, Avg: 6, Max: 7, Jitter: 1},
		},
	}
	if n := Sanitize(rep); n != 9 {
		t.Fatalf("dropped %d, want 9", n)
	}
	if rep.Sys.Os != "" || rep.Sys.Hostname != "ok" || rep.Cpu != nil || rep.Load == nil || rep.Mem != nil {
		t.Fatalf("scalars: %+v", rep)
	}
	big := &pb.Report{Load: &pb.Load{L1: 1, Threads: 2e7}, Sockets: &pb.Sockets{Tcp: 5, Udp: 2e7}}
	if n := Sanitize(big); n != 2 || big.Load == nil || big.Load.Threads != 0 || big.Sockets != nil {
		t.Fatalf("counts: dropped %d, %+v", n, big)
	}
	if len(rep.Disks) != 1 || len(rep.Net) != 1 || len(rep.Traffic) != 1 || len(rep.Pings) != 2 {
		t.Fatalf("lists: disks=%d net=%d traffic=%d pings=%d", len(rep.Disks), len(rep.Net), len(rep.Traffic), len(rep.Pings))
	}
	if rep.Pings[0].Target != "b" || rep.Pings[1].Target != "e" {
		t.Fatalf("pings kept: %v %v", rep.Pings[0].Target, rep.Pings[1].Target)
	}
}

// TestAgentSenderToStore runs the real agent sender against the real
// server path: a report too big for one packet must arrive split, be
// acknowledged, and merge back into one row per table.
func TestAgentSenderToStore(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	st, err := store.Open(filepath.Join(t.TempDir(), "probe.db"), store.Options{Location: loc,
		Retention: store.Retention{Raw: 48 * time.Hour, M5: 720 * time.Hour, H1: 9600 * time.Hour}, Log: discard})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SyncNodes([]string{node}); err != nil {
		t.Fatal(err)
	}
	s := start(t, st)

	snd, err := report.NewSender(s.Addr().String(), node, token, discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go snd.Run(ctx)

	ts := time.Now().Unix()
	rep := &pb.Report{Ts: ts,
		Cpu:   &pb.CPU{Usage: 33},
		Mem:   &pb.Mem{Total: 1000, Used: 500},
		Sys:   &pb.SysInfo{Hostname: "hk-1", Os: "Debian"},
		Net:   []*pb.NetRate{{Iface: "eth0", RxRate: 100, TxRate: 200}},
		Disks: []*pb.Disk{{Mount: "/", Total: 100, Used: 50, Avail: 50}},
		Traffic: []*pb.IfaceTraffic{{Iface: "eth0",
			Cur: &pb.Period{Start: "2026-09-01", Rx: 5 << 30, Tx: 1 << 30}}},
	}
	for i := range 40 {
		rep.Pings = append(rep.Pings, &pb.Ping{Target: fmt.Sprintf("peer-with-a-long-name-%02d", i),
			Addr: "203.0.113.1", Sent: 10, Lost: 1, Min: 1, Avg: 2, Max: 3, Jitter: 0.5})
	}
	aead, _ := wire.NewAEAD(token, node)
	pkts, _ := report.Packetize(aead, node, proto.Clone(rep).(*pb.Report))
	if len(pkts) < 2 {
		t.Fatalf("test report not split (%d packets)", len(pkts))
	}
	snd.Enqueue(rep)

	deadline := time.Now().Add(5 * time.Second)
	for {
		queued, acked := snd.Stats()
		if queued == 0 && acked == uint64(len(pkts)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued=%d acked=%d of %d", queued, acked, len(pkts))
		}
		time.Sleep(20 * time.Millisecond)
	}

	status, err := st.Status(context.Background(), node)
	if err != nil || status == nil {
		t.Fatal(status, err)
	}
	if *status.CPU != 33 || *status.MemUsed != 500 || status.Sys.Hostname != "hk-1" ||
		len(status.Net) != 1 || len(status.Disks) != 1 || status.Traffic.RX != 5<<30 {
		t.Fatalf("status: %+v", status)
	}
	links, err := st.Matrix(context.Background(), 5*time.Minute)
	if err != nil || len(links) != 40 {
		t.Fatalf("links = %d, %v", len(links), err)
	}
}

// The default ingest address has an empty host so IPv6 agents get through.
func TestDualStack(t *testing.T) {
	s, err := Listen(":0", []config.Node{{ID: node, Token: token}}, &fakeWriter{}, discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	port := s.Addr().(*net.UDPAddr).Port
	a := aead(t, token)
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.ParseIP(host), Port: port})
		if err != nil {
			t.Skipf("%s: %v", host, err)
		}
		defer c.Close()
		pkt := sealReport(t, a, wire.TypeReport, &pb.Report{Ts: time.Now().Unix(), Id: uint64(len(host))})
		if exchange(t, c, pkt) == nil {
			t.Errorf("no ack over %s", host)
		}
	}
}

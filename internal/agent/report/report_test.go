package report

import (
	"context"
	"crypto/cipher"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	pb "vpsprobe/internal/proto/probev1"
	"vpsprobe/internal/wire"
)

const (
	testNode  = "hk-1"
	testToken = "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func testAEAD(t *testing.T) cipher.AEAD {
	t.Helper()
	a, err := wire.NewAEAD(testToken, testNode)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func open(t *testing.T, aead cipher.AEAD, pkt []byte) *pb.Report {
	t.Helper()
	h, err := wire.ParseHeader(pkt)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := h.Open(aead)
	if err != nil {
		t.Fatal(err)
	}
	var r pb.Report
	if err := proto.Unmarshal(pt, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func typical() *pb.Report {
	r := &pb.Report{
		Ts:      time.Now().Unix(),
		Cpu:     &pb.CPU{Usage: 12.5, Steal: 0.3},
		Load:    &pb.Load{L1: 0.1, L5: 0.2, L15: 0.3, Threads: 312},
		Mem:     &pb.Mem{Total: 2 << 30, Used: 1 << 30},
		Sockets: &pb.Sockets{Tcp: 42, Udp: 7, TcpTw: 12},
		Net:     []*pb.NetRate{{Iface: "eth0", RxRate: 123456, TxRate: 654321}},
		Traffic: []*pb.IfaceTraffic{{Iface: "eth0",
			Cur:  &pb.Period{Start: "2026-09-01", Rx: 51234567890, Tx: 40123456789},
			Prev: &pb.Period{Start: "2026-08-01", Rx: 61234567890, Tx: 50123456789}}},
		Disks: []*pb.Disk{{Mount: "/", Total: 40 << 30, Used: 12 << 30, Avail: 26 << 30, InodePct: 8}},
		Sys: &pb.SysInfo{Hostname: "hk-1.example.com", Os: "Debian GNU/Linux 12 (bookworm)",
			Kernel: "6.1.0-25-amd64", Arch: "amd64", Cores: 2, BootTime: 1789000000, Uptime: 1000000, AgentVersion: "v0.1.0"},
	}
	for i := range 10 {
		r.Pings = append(r.Pings, &pb.Ping{Target: fmt.Sprintf("peer-%d", i), Addr: fmt.Sprintf("203.0.113.%d", i),
			Sent: 10, Lost: 1, Min: 31.2, Avg: 32.5, Max: 40.1, Jitter: 1.2})
	}
	return r
}

func TestTypicalReportFitsOnePacket(t *testing.T) {
	aead := testAEAD(t)
	pkts, err := Packetize(aead, testNode, typical())
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) != 1 {
		t.Fatalf("typical report split into %d packets", len(pkts))
	}
	t.Logf("typical report: %d bytes on the wire", len(pkts[0].Bytes))
	got := open(t, aead, pkts[0].Bytes)
	if got.Id != pkts[0].ID || len(got.Pings) != 10 || got.Sys.Hostname != "hk-1.example.com" {
		t.Fatalf("round trip mismatch: %v", got)
	}
}

func TestLargeReportSplits(t *testing.T) {
	aead := testAEAD(t)
	r := typical()
	for i := range 40 {
		r.Pings = append(r.Pings, &pb.Ping{Target: fmt.Sprintf("extra-peer-with-long-name-%d", i),
			Addr: "2001:db8:1234:5678:9abc:def0:1234:5678", Sent: 10, Min: 1, Avg: 2, Max: 3})
	}
	for i := range 10 {
		r.Disks = append(r.Disks, &pb.Disk{Mount: fmt.Sprintf("/mnt/volume-%d", i), Total: 1 << 40, Used: 1 << 39})
	}
	orig := proto.Clone(r).(*pb.Report)

	pkts, err := Packetize(aead, testNode, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkts) < 2 {
		t.Fatalf("expected a split, got %d packet(s)", len(pkts))
	}
	merged := &pb.Report{}
	ids := map[uint64]bool{}
	for _, p := range pkts {
		if len(p.Bytes) > wire.MaxPacket {
			t.Fatalf("packet of %d bytes", len(p.Bytes))
		}
		got := open(t, aead, p.Bytes)
		if got.Ts != orig.Ts {
			t.Fatalf("piece ts = %d", got.Ts)
		}
		if ids[got.Id] {
			t.Fatal("duplicate id across pieces")
		}
		ids[got.Id] = true
		got.Id = 0
		proto.Merge(merged, got)
	}
	orig.Id = 0
	if !proto.Equal(merged, orig) {
		t.Fatal("merged pieces differ from original report")
	}
}

// fakeServer acks every valid report and records ids.
type fakeServer struct {
	conn *net.UDPConn
	aead cipher.AEAD

	mu     sync.Mutex
	seen   map[uint64]int
	muted  bool
	nvalid int
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeServer{conn: conn, aead: testAEAD(t), seen: map[uint64]int{}}
	go fs.serve()
	t.Cleanup(func() { conn.Close() })
	return fs
}

func (f *fakeServer) serve() {
	buf := make([]byte, 2048)
	for {
		n, from, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		h, err := wire.ParseHeader(buf[:n])
		if err != nil || h.Type != wire.TypeReport {
			continue
		}
		pt, err := h.Open(f.aead)
		if err != nil {
			continue
		}
		var r pb.Report
		if proto.Unmarshal(pt, &r) != nil {
			continue
		}
		f.mu.Lock()
		f.nvalid++
		f.seen[r.Id]++
		muted := f.muted
		f.mu.Unlock()
		if muted {
			continue
		}
		ack, _ := proto.Marshal(&pb.Ack{Ids: []uint64{r.Id}})
		pkt, _ := wire.Seal(f.aead, wire.TypeAck, testNode, ack)
		f.conn.WriteToUDP(pkt, from)
	}
}

func (f *fakeServer) setMuted(m bool) {
	f.mu.Lock()
	f.muted = m
	f.mu.Unlock()
}

func (f *fakeServer) distinct() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func startSender(t *testing.T, addr string) *Sender {
	t.Helper()
	s, err := NewSender(addr, testNode, testToken, discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// Wait for the socket before enqueueing so the first send isn't queued.
	waitFor(t, "dial", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.conn != nil
	})
	return s
}

func TestSenderDeliversAndDequeues(t *testing.T) {
	srv := newFakeServer(t)
	s := startSender(t, srv.conn.LocalAddr().String())
	for i := range 3 {
		r := typical()
		r.Ts += int64(i * 10)
		s.Enqueue(r)
	}
	waitFor(t, "acks", func() bool { q, acked := s.Stats(); return q == 0 && acked == 3 })
	if srv.distinct() != 3 {
		t.Fatalf("server saw %d reports", srv.distinct())
	}
}

func TestSenderBacklogDrainsAfterOutage(t *testing.T) {
	srv := newFakeServer(t)
	s := startSender(t, srv.conn.LocalAddr().String())
	now := time.Now()
	s.mu.Lock()
	s.now = func() time.Time { return now }
	s.mu.Unlock()
	advance := func(d time.Duration) {
		s.mu.Lock()
		now = now.Add(d)
		s.mu.Unlock()
	}

	srv.setMuted(true)
	for i := range 30 {
		r := typical()
		r.Ts = now.Unix() + int64(i)
		s.Enqueue(r)
	}
	waitFor(t, "reports arrive while muted", func() bool { return srv.distinct() == 30 })
	if q, _ := s.Stats(); q != 30 {
		t.Fatalf("queued = %d", q)
	}

	// Server back. While not alive only the newest is retried; its ack
	// flips the sender to alive and the rest drain.
	srv.setMuted(false)
	advance(retryAfter)
	s.pump()
	waitFor(t, "first ack", func() bool { q, _ := s.Stats(); return q == 29 })
	for range 5 {
		s.pump()
	}
	waitFor(t, "drain", func() bool { q, _ := s.Stats(); return q == 0 })
}

func TestSenderProbesOnlyNewestWhileDown(t *testing.T) {
	// Nothing listens here; sends go nowhere.
	s, err := NewSender("127.0.0.1:9", testNode, testToken, discard)
	if err != nil {
		t.Fatal(err)
	}
	s.redial()
	t.Cleanup(func() { s.conn.Close() })
	now := time.Now()
	s.now = func() time.Time { return now }
	s.started = now
	for i := range 5 {
		r := typical()
		r.Ts = now.Unix() + int64(i)
		s.Enqueue(r)
	}
	now = now.Add(retryAfter)
	s.pump()
	resent := 0
	for _, it := range s.queue {
		if it.lastSent.Equal(now) {
			resent++
		}
	}
	if resent != 1 || !s.queue[len(s.queue)-1].lastSent.Equal(now) {
		t.Fatalf("resent %d while down; want only the newest", resent)
	}
}

func TestSenderQueueBounds(t *testing.T) {
	s, _ := NewSender("127.0.0.1:9", testNode, testToken, discard)
	now := time.Now()
	s.now = func() time.Time { return now }
	for i := range queueCap + 50 {
		r := typical()
		r.Ts = now.Unix() - int64(queueCap+50-i)
		s.Enqueue(r)
	}
	if q, _ := s.Stats(); q != queueCap {
		t.Fatalf("queue = %d, want %d", q, queueCap)
	}
	if s.queue[0].TS != now.Unix()-int64(queueCap) {
		t.Fatalf("oldest kept ts = %d; oldest entries should be dropped first", s.queue[0].TS)
	}
	now = now.Add(maxAge + time.Hour)
	s.pump()
	if q, _ := s.Stats(); q != 0 {
		t.Fatalf("expired entries kept: %d", q)
	}
}

func TestSenderIgnoresForeignAcks(t *testing.T) {
	s, _ := NewSender("127.0.0.1:9", testNode, testToken, discard)
	r := typical()
	s.Enqueue(r)
	id := s.queue[0].ID
	ack, _ := proto.Marshal(&pb.Ack{Ids: []uint64{id}})

	other, _ := wire.NewAEAD("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", testNode)
	forged, _ := wire.Seal(other, wire.TypeAck, testNode, ack)
	s.handleAck(forged)
	// A Report-typed packet under the right key is not an Ack.
	wrongType, _ := wire.Seal(testAEAD(t), wire.TypeReport, testNode, ack)
	s.handleAck(wrongType)
	s.handleAck([]byte("garbage"))
	if q, _ := s.Stats(); q != 1 {
		t.Fatal("forged ack removed an item")
	}
	good, _ := wire.Seal(testAEAD(t), wire.TypeAck, testNode, ack)
	s.handleAck(good)
	if q, _ := s.Stats(); q != 0 {
		t.Fatal("valid ack ignored")
	}
}

// A report queued before the socket exists goes out on the first pump after
// the dial, not retryAfter later.
func TestSenderSendsQueuedReportAfterDial(t *testing.T) {
	s, err := NewSender("127.0.0.1:9", testNode, testToken, discard)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.now = func() time.Time { return now }
	s.started = now
	s.Enqueue(typical())
	if !s.queue[0].lastSent.IsZero() {
		t.Fatal("marked as sent without a socket")
	}
	s.redial()
	t.Cleanup(func() { s.conn.Close() })
	now = now.Add(pumpEvery)
	s.pump()
	if !s.queue[0].lastSent.Equal(now) {
		t.Fatalf("not sent on the first pump after dialing (last sent %v)", s.queue[0].lastSent)
	}
}

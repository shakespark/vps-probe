package ping

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/shakespark/vps-probe/internal/echo"
	"github.com/shakespark/vps-probe/internal/peer"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestSummarize(t *testing.T) {
	st := Stats{}
	summarize(&st, []float64{10, 14, 12, 20})
	if st.Min != 10 || st.Max != 20 || st.Avg != 14 {
		t.Fatalf("got %+v", st)
	}
	// |14-10| + |12-14| + |20-12| = 14, over 3 gaps
	if math.Abs(st.Jitter-14.0/3) > 1e-9 {
		t.Fatalf("jitter = %v", st.Jitter)
	}
	empty := Stats{}
	summarize(&empty, nil)
	if empty != (Stats{}) {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestLoopback(t *testing.T) {
	p, err := New([]peer.Peer{{Name: "self", Addr: "127.0.0.1"}}, 100*time.Millisecond, 500*time.Millisecond, discard)
	if err != nil {
		t.Skipf("no ICMP socket in this environment: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	time.Sleep(1200 * time.Millisecond)
	cancel()
	<-done

	st := p.Snapshot(time.Now())
	if len(st) != 1 {
		t.Fatalf("got %d stats", len(st))
	}
	s := st[0]
	if s.Target != "self" || s.Addr != "127.0.0.1" {
		t.Fatalf("got %+v", s)
	}
	if s.Sent < 5 || s.Lost != 0 {
		t.Fatalf("sent=%d lost=%d", s.Sent, s.Lost)
	}
	if s.Min <= 0 || s.Avg < s.Min || s.Max < s.Avg || s.Max > 100 {
		t.Fatalf("implausible rtt: %+v", s)
	}
	// Everything was consumed; a second snapshot is empty.
	if again := p.Snapshot(time.Now()); again[0].Sent != 0 {
		t.Fatalf("second snapshot = %+v", again[0])
	}
}

func TestUnreachableCountsAsLost(t *testing.T) {
	// TEST-NET-1 is never routed to a real host; replies won't come back.
	p, err := New([]peer.Peer{{Name: "void", Addr: "192.0.2.1"}}, 50*time.Millisecond, 200*time.Millisecond, discard)
	if err != nil {
		t.Skipf("no ICMP socket: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done

	// Probes younger than the timeout stay pending.
	first := p.Snapshot(time.Now())[0]
	later := p.Snapshot(time.Now().Add(time.Second))[0]
	total := first.Sent + later.Sent
	if total < 3 {
		t.Fatalf("sent only %d", total)
	}
	if first.Lost+later.Lost != total {
		t.Fatalf("lost %d of %d", first.Lost+later.Lost, total)
	}
	if later.Avg != 0 {
		t.Fatalf("rtt reported with no replies: %+v", later)
	}
}

func TestUnresolvableHostname(t *testing.T) {
	p, err := New([]peer.Peer{{Name: "bad", Addr: "no-such-host.invalid"}, {Name: "self", Addr: "127.0.0.1"}},
		time.Second, time.Second, discard)
	if err != nil {
		t.Skipf("no ICMP socket: %v", err)
	}
	st := p.Snapshot(time.Now())
	if st[0].Target != "bad" || st[0].Addr != "" || st[0].Sent != 0 {
		t.Fatalf("got %+v", st[0])
	}
}

func TestFailedResolveBacksOff(t *testing.T) {
	calls := 0
	orig := resolveFn
	t.Cleanup(func() { resolveFn = orig })
	resolveFn = func(ctx context.Context, addr string) (netip.Addr, error) {
		calls++
		if addr == "good.example" {
			return netip.MustParseAddr("192.0.2.10"), nil
		}
		return netip.Addr{}, errors.New("dns down")
	}
	p := &Pinger{log: discard, peers: []*target{
		{Peer: peer.Peer{Name: "bad", Addr: "bad.example"}, host: "bad.example"},
		{Peer: peer.Peer{Name: "good", Addr: "good.example"}, host: "good.example"},
	}}
	now := time.Now()
	p.maybeResolve(context.Background(), now)
	if calls != 2 {
		t.Fatalf("initial calls = %d", calls)
	}
	// Within the retry window nothing is looked up again.
	p.maybeResolve(context.Background(), now.Add(time.Second))
	p.maybeResolve(context.Background(), now.Add(resolveRetry-time.Second))
	if calls != 2 {
		t.Fatalf("retried too early: calls = %d", calls)
	}
	// The failed one retries after resolveRetry; the good one waits longer.
	p.maybeResolve(context.Background(), now.Add(resolveRetry))
	if calls != 3 {
		t.Fatalf("after retry window: calls = %d", calls)
	}
	p.maybeResolve(context.Background(), now.Add(resolveEvery))
	if calls != 5 {
		t.Fatalf("after refresh window: calls = %d", calls)
	}
	if p.peers[1].ip != netip.MustParseAddr("192.0.2.10") {
		t.Fatalf("good peer ip = %v", p.peers[1].ip)
	}
}

// fakeResolver answers every query on a loopback UDP port, echoing the ID
// with the response bit set, unless mute is set.
func fakeResolver(t *testing.T, mute bool) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			if mute || n < 12 {
				continue
			}
			resp := append([]byte(nil), buf[:n]...)
			resp[2] |= 0x80
			c.WriteTo([]byte{0, 0, 0}, from) // garbage is ignored
			c.WriteTo(resp, from)
		}
	}()
	return c.LocalAddr().String()
}

func runFor(p *Pinger, d time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	time.Sleep(d)
	cancel()
	<-done
}

func TestDNS(t *testing.T) {
	addr := fakeResolver(t, false)
	// DNS peers need no ICMP socket, so this runs everywhere.
	p, err := New([]peer.Peer{{Name: "cf", Addr: addr, Type: peer.DNS}}, 100*time.Millisecond, 500*time.Millisecond, discard)
	if err != nil {
		t.Fatal(err)
	}
	if p.icmp != nil {
		t.Fatal("ICMP socket opened for DNS-only peers")
	}
	runFor(p, 1200*time.Millisecond)
	s := p.Snapshot(time.Now())[0]
	if s.Target != "cf" || s.Addr != addr {
		t.Fatalf("got %+v", s)
	}
	if s.Sent < 5 || s.Lost != 0 {
		t.Fatalf("sent=%d lost=%d", s.Sent, s.Lost)
	}
	if s.Min <= 0 || s.Avg < s.Min || s.Max < s.Avg || s.Max > 100 {
		t.Fatalf("implausible rtt: %+v", s)
	}
	// At most the query sent just before shutdown is unanswered.
	if n := len(p.waiting); n > 1 {
		t.Fatalf("answered queries still waiting: %d", n)
	}
}

func TestDNSNoAnswerIsLost(t *testing.T) {
	addr := fakeResolver(t, true)
	p, err := New([]peer.Peer{{Name: "cf", Addr: addr, Type: peer.DNS}}, 50*time.Millisecond, 200*time.Millisecond, discard)
	if err != nil {
		t.Fatal(err)
	}
	runFor(p, 300*time.Millisecond)
	s := p.Snapshot(time.Now().Add(time.Second))[0]
	if s.Sent < 3 || s.Lost != s.Sent || s.Avg != 0 {
		t.Fatalf("got %+v", s)
	}
	if len(p.waiting) != 0 {
		t.Fatalf("timed-out queries still waiting: %d", len(p.waiting))
	}
}

func TestDNSBadAddr(t *testing.T) {
	for _, a := range []string{"1.1.1.1", "1.1.1.1:0", "1.1.1.1:dns", "1.1.1.1:70000"} {
		if _, err := New([]peer.Peer{{Name: "x", Addr: a, Type: peer.DNS}}, time.Second, time.Second, discard); err == nil {
			t.Errorf("%q accepted", a)
		}
	}
}

func TestDNSQuery(t *testing.T) {
	q := dnsQuery(0xabcd)
	if len(q) != 33 || q[0] != 0xab || q[1] != 0xcd || q[2]&0x80 != 0 || q[5] != 1 {
		t.Fatalf("query % x", q)
	}
}

func TestEcho(t *testing.T) {
	key := []byte("abcdefghijklmnopqrstuvwxyz0123456789")
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go (&echo.Responder{Key: key, Log: discard}).Serve(c)
	t.Cleanup(func() { c.Close() })
	addr := c.LocalAddr().String()

	p, err := New([]peer.Peer{
		{Name: "tun", Addr: addr, Type: peer.Echo, Key: string(key)},
		{Name: "wrong-key", Addr: addr, Type: peer.Echo, Key: "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},
	}, 100*time.Millisecond, 500*time.Millisecond, discard)
	if err != nil {
		t.Fatal(err)
	}
	runFor(p, 1200*time.Millisecond)
	st := p.Snapshot(time.Now().Add(time.Second))
	// Lost <= 1: the request sent just before shutdown never gets its reply.
	if s := st[0]; s.Sent < 5 || s.Lost > 1 || s.Addr != addr || s.Min <= 0 || s.Max > 100 {
		t.Fatalf("tun: %+v", s)
	}
	// The responder ignores a request signed with another key.
	if s := st[1]; s.Sent < 5 || s.Lost != s.Sent {
		t.Fatalf("wrong-key: %+v", s)
	}
}

func TestEchoTCP(t *testing.T) {
	key := []byte("abcdefghijklmnopqrstuvwxyz0123456789")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go (&echo.Responder{Key: key, Log: discard}).ServeTCP(ln)
	t.Cleanup(func() { ln.Close() })
	addr := ln.Addr().String()

	// Nothing listens here: every probe is lost, and nothing blocks.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()

	p, err := New([]peer.Peer{
		{Name: "tun", Addr: addr, Type: peer.EchoTCP, Key: string(key)},
		{Name: "wrong-key", Addr: addr, Type: peer.EchoTCP, Key: "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"},
		{Name: "dead", Addr: deadAddr, Type: peer.EchoTCP, Key: string(key)},
	}, 100*time.Millisecond, 500*time.Millisecond, discard)
	if err != nil {
		t.Fatal(err)
	}
	runFor(p, 1200*time.Millisecond)
	st := p.Snapshot(time.Now().Add(time.Second))
	// Lost <= 2: the probe sent while connecting, and the one sent just
	// before shutdown.
	if s := st[0]; s.Sent < 5 || s.Lost > 2 || s.Addr != addr || s.Min <= 0 || s.Max > 100 {
		t.Fatalf("tun: %+v", s)
	}
	// The responder hangs up on a request signed with another key.
	if s := st[1]; s.Sent < 5 || s.Lost != s.Sent {
		t.Fatalf("wrong-key: %+v", s)
	}
	if s := st[2]; s.Sent < 5 || s.Lost != s.Sent {
		t.Fatalf("dead: %+v", s)
	}
}

// A connection that ends is replaced, and probes are answered again.
func TestEchoTCPReconnects(t *testing.T) {
	key := []byte("abcdefghijklmnopqrstuvwxyz0123456789")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	// The first connection is dropped after one reply; later ones are served.
	conns := make(chan net.Conn, 8)
	go func() {
		for first := true; ; first = false {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
			go func() {
				defer c.Close()
				buf := make([]byte, echo.Size)
				for {
					if _, err := io.ReadFull(c, buf); err != nil {
						return
					}
					rep, err := echo.Answer(key, buf, time.Now())
					if err != nil {
						return
					}
					c.Write(rep)
					if first {
						return
					}
				}
			}()
		}
	}()

	p, err := New([]peer.Peer{{Name: "tun", Addr: ln.Addr().String(), Type: peer.EchoTCP, Key: string(key)}},
		50*time.Millisecond, 300*time.Millisecond, discard)
	if err != nil {
		t.Fatal(err)
	}
	runFor(p, 1200*time.Millisecond)
	s := p.Snapshot(time.Now().Add(time.Second))[0]
	if n := len(conns); n < 2 {
		t.Fatalf("%d connections, want a reconnect", n)
	}
	if got := s.Sent - s.Lost; got < 10 {
		t.Fatalf("only %d of %d answered after the reconnect", got, s.Sent)
	}
}

func TestBadPeers(t *testing.T) {
	for name, pr := range map[string]peer.Peer{
		"type":      {Name: "x", Addr: "1.1.1.1", Type: "tcp"},
		"short key": {Name: "x", Addr: "127.0.0.1:39527", Type: peer.Echo, Key: "short"},
		"echo port": {Name: "x", Addr: "127.0.0.1", Type: peer.Echo, Key: "abcdefghijklmnopqrstuvwxyz0123456789"},
		"tcp key":   {Name: "x", Addr: "127.0.0.1:39527", Type: peer.EchoTCP},
		"tcp port":  {Name: "x", Addr: "127.0.0.1", Type: peer.EchoTCP, Key: "abcdefghijklmnopqrstuvwxyz0123456789"},
		"dns key":   {Name: "x", Addr: "127.0.0.1:53", Type: peer.DNS, Key: "abcdefghijklmnopqrstuvwxyz0123456789"},
	} {
		if _, err := New([]peer.Peer{pr}, time.Second, time.Second, discard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// slowResolver answers every query after delay.
func slowResolver(t *testing.T, delay time.Duration) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			resp := append([]byte(nil), buf[:n]...)
			resp[2] |= 0x80
			time.AfterFunc(delay, func() { c.WriteTo(resp, from) })
		}
	}()
	return c.LocalAddr().String()
}

func TestLateReplyIsLost(t *testing.T) {
	// Replies come back after the timeout but before the next snapshot:
	// they are losses, not 250 ms round trips.
	addr := slowResolver(t, 250*time.Millisecond)
	p, err := New([]peer.Peer{{Name: "cf", Addr: addr, Type: peer.DNS}}, 50*time.Millisecond, 100*time.Millisecond, discard)
	if err != nil {
		t.Fatal(err)
	}
	runFor(p, 600*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // every reply has arrived
	s := p.Snapshot(time.Now())[0]
	if s.Sent < 5 || s.Lost != s.Sent || s.Max != 0 {
		t.Fatalf("late replies counted: %+v", s)
	}
}

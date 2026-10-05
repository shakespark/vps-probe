package echo

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"
)

var (
	key     = []byte("abcdefghijklmnopqrstuvwxyz0123456789")
	discard = slog.New(slog.NewTextHandler(io.Discard, nil))
)

func TestRoundTrip(t *testing.T) {
	now := time.Now()
	req := Request(key, 0xbeef, now)
	if len(req) != Size {
		t.Fatalf("size %d", len(req))
	}
	rep, err := Answer(key, req, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep) > len(req) {
		t.Fatal("reply larger than request")
	}
	if seq, ok := ParseReply(key, rep); !ok || seq != 0xbeef {
		t.Fatalf("seq=%x ok=%v", seq, ok)
	}
	// A request is not a reply, and a reply is not a request.
	if _, ok := ParseReply(key, req); ok {
		t.Fatal("request parsed as reply")
	}
	if _, err := Answer(key, rep, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reply answered: %v", err)
	}
}

func TestRejects(t *testing.T) {
	now := time.Now()
	req := Request(key, 1, now)
	other := []byte("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	if _, err := Answer(other, req, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong key: %v", err)
	}
	for i := range req {
		bad := append([]byte(nil), req...)
		bad[i] ^= 1
		if _, err := Answer(key, bad, now); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bit flip at %d accepted: %v", i, err)
		}
	}
	if _, err := Answer(key, append(req, 0), now); !errors.Is(err, ErrInvalid) {
		t.Fatal("padded request accepted")
	}
	if _, err := Answer(key, req, now.Add(Window+time.Second)); !errors.Is(err, ErrClock) {
		t.Fatalf("stale: %v", err)
	}
	if _, err := Answer(key, req, now.Add(-Window-time.Second)); !errors.Is(err, ErrClock) {
		t.Fatalf("future: %v", err)
	}
}

func serve(t *testing.T, r *Responder) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Serve(c) }()
	t.Cleanup(func() {
		c.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return c.LocalAddr().String()
}

// exchange sends each packet and reports how many replies came back.
func exchange(t *testing.T, addr string, pkts ...[]byte) int {
	t.Helper()
	c, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, p := range pkts {
		c.Write(p)
	}
	got := 0
	buf := make([]byte, 100)
	for {
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, err := c.Read(buf)
		if err != nil {
			return got
		}
		if _, ok := ParseReply(key, buf[:n]); !ok {
			t.Fatalf("bad reply % x", buf[:n])
		}
		got++
	}
}

func TestResponder(t *testing.T) {
	addr := serve(t, &Responder{Key: key, Log: discard})
	now := time.Now()
	junk := []byte("hello")
	stale := Request(key, 3, now.Add(-time.Hour))
	if got := exchange(t, addr, Request(key, 1, now), junk, stale, Request(key, 2, now)); got != 2 {
		t.Fatalf("got %d replies, want 2", got)
	}
}

func TestResponderAllow(t *testing.T) {
	addr := serve(t, &Responder{Key: key, Log: discard, Allow: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}})
	if got := exchange(t, addr, Request(key, 1, time.Now())); got != 0 {
		t.Fatalf("source outside allow answered (%d)", got)
	}
	addr = serve(t, &Responder{Key: key, Log: discard, Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	if got := exchange(t, addr, Request(key, 1, time.Now())); got != 1 {
		t.Fatalf("allowed source got %d replies", got)
	}
}

func TestResponderRateCap(t *testing.T) {
	addr := serve(t, &Responder{Key: key, Log: discard, MaxPPS: 3})
	var pkts [][]byte
	for i := range 10 {
		pkts = append(pkts, Request(key, uint16(i), time.Now()))
	}
	// Ten in one burst; at most one second boundary can fall inside it.
	if got := exchange(t, addr, pkts...); got < 3 || got > 6 {
		t.Fatalf("got %d replies with a cap of 3/s", got)
	}
}

func serveTCP(t *testing.T, r *Responder) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.ServeTCP(ln) }()
	t.Cleanup(func() {
		ln.Close()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return ln.Addr().String()
}

// exchangeTCP writes the packets on one connection and reports how many
// replies came back before the responder closed it or went quiet.
func exchangeTCP(t *testing.T, addr string, pkts ...[]byte) (got int, closed bool) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, p := range pkts {
		c.Write(p)
	}
	buf := make([]byte, Size)
	for {
		c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if _, err := io.ReadFull(c, buf); err != nil {
			// EOF, or a reset when it hung up with requests unread.
			var ne net.Error
			return got, !(errors.As(err, &ne) && ne.Timeout())
		}
		if _, ok := ParseReply(key, buf); !ok {
			t.Fatalf("bad reply % x", buf)
		}
		got++
	}
}

func TestResponderTCP(t *testing.T) {
	addr := serveTCP(t, &Responder{Key: key, Log: discard})
	now := time.Now()
	if got, closed := exchangeTCP(t, addr, Request(key, 1, now), Request(key, 2, now), Request(key, 3, now)); got != 3 || closed {
		t.Fatalf("valid requests: %d replies, closed %v", got, closed)
	}
	// The first request that is not answered ends the connection, with
	// nothing written for it or after it.
	junk := make([]byte, Size)
	if got, closed := exchangeTCP(t, addr, Request(key, 1, now), junk, Request(key, 2, now)); got != 1 || !closed {
		t.Fatalf("junk: %d replies, closed %v", got, closed)
	}
	if got, closed := exchangeTCP(t, addr, Request(key, 1, now.Add(-time.Hour))); got != 0 || !closed {
		t.Fatalf("stale: %d replies, closed %v", got, closed)
	}
	if got, closed := exchangeTCP(t, addr, []byte("GET / HTTP/1.0\r\n\r\n")); got != 0 || closed {
		t.Fatalf("short junk: %d replies, closed %v (want silence)", got, closed)
	}
	// A health check that connects and hangs up doesn't disturb anything.
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if got, _ := exchangeTCP(t, addr, Request(key, 9, time.Now())); got != 1 {
		t.Fatalf("after a bare connect: %d replies", got)
	}
}

func TestResponderTCPAllow(t *testing.T) {
	addr := serveTCP(t, &Responder{Key: key, Log: discard, Allow: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}})
	if got, closed := exchangeTCP(t, addr, Request(key, 1, time.Now())); got != 0 || !closed {
		t.Fatalf("source outside allow: %d replies, closed %v", got, closed)
	}
}

// Closing the listener ends ServeTCP even with a connection still open.
func TestResponderTCPShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- (&Responder{Key: key, Log: discard}).ServeTCP(ln) }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write(Request(key, 1, time.Now()))
	if _, err := io.ReadFull(c, make([]byte, Size)); err != nil {
		t.Fatal(err)
	}
	ln.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeTCP still running")
	}
}

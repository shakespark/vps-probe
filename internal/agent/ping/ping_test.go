package ping

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/netip"
	"testing"
	"time"
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
	p, err := New([]Peer{{Name: "self", Addr: "127.0.0.1"}}, 100*time.Millisecond, 500*time.Millisecond, discard)
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
	p, err := New([]Peer{{Name: "void", Addr: "192.0.2.1"}}, 50*time.Millisecond, 200*time.Millisecond, discard)
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
	p, err := New([]Peer{{Name: "bad", Addr: "no-such-host.invalid"}, {Name: "self", Addr: "127.0.0.1"}},
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
	p := &Pinger{log: discard, peers: []*peerState{
		{Peer: Peer{Name: "bad", Addr: "bad.example"}},
		{Peer: Peer{Name: "good", Addr: "good.example"}},
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

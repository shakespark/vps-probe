// Package ping measures ICMP echo latency to a fixed set of peers.
//
// It prefers unprivileged datagram ICMP sockets (net.ipv4.ping_group_range),
// falling back to raw sockets, which need CAP_NET_RAW. With datagram sockets
// the kernel rewrites the echo ID and filters replies per socket, so replies
// are matched on sequence number and source address only.
package ping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	protoICMP   = 1
	protoICMPv6 = 58

	resolveEvery = 10 * time.Minute // refresh a good address
	resolveRetry = 30 * time.Second // retry after a failed lookup
	resolveCheck = 5 * time.Second
	payloadSize  = 16
)

// Peer is a configured target.
type Peer struct {
	Name string
	Addr string // IP literal or hostname
}

// Stats summarizes the probes to one peer that completed in a window.
type Stats struct {
	Target string
	Addr   string // resolved IP, empty if unresolved
	Sent   int
	Lost   int
	Min    float64 // milliseconds, over replies only
	Avg    float64
	Max    float64
	Jitter float64 // mean |rtt[i] - rtt[i-1]|
}

type probe struct {
	ip   netip.Addr
	seq  uint16
	sent time.Time
	rtt  time.Duration
	got  bool
}

type peerState struct {
	Peer
	ip         netip.Addr
	resolvedAt time.Time
	probes     []*probe
}

type family struct {
	conn     *icmp.PacketConn
	dgram    bool
	echoType icmp.Type
	replyTyp icmp.Type
	proto    int
}

// Pinger sends one echo per peer per interval.
type Pinger struct {
	interval, timeout time.Duration
	log               *slog.Logger
	id                int

	mu       sync.Mutex
	peers    []*peerState
	v4, v6   *family
	seq      uint16
	inflight map[inflightKey]*probe
}

type inflightKey struct {
	ip  netip.Addr
	seq uint16
}

// New opens the ICMP sockets needed for the peers. It fails only if no
// socket could be opened for any address family in use.
func New(peers []Peer, interval, timeout time.Duration, log *slog.Logger) (*Pinger, error) {
	p := &Pinger{
		interval: interval,
		timeout:  timeout,
		log:      log,
		id:       os.Getpid() & 0xffff,
		inflight: map[inflightKey]*probe{},
	}
	for _, pr := range peers {
		p.peers = append(p.peers, &peerState{Peer: pr})
	}
	p.maybeResolve(context.Background(), time.Now())

	var errs []error
	if f, err := openFamily(false); err == nil {
		p.v4 = f
		log.Info("ping: ipv4 socket open", "unprivileged", f.dgram)
	} else {
		errs = append(errs, err)
	}
	if p.needV6() {
		if f, err := openFamily(true); err == nil {
			p.v6 = f
			log.Info("ping: ipv6 socket open", "unprivileged", f.dgram)
		} else {
			errs = append(errs, err)
		}
	}
	if p.v4 == nil && p.v6 == nil {
		return nil, fmt.Errorf("ping: no ICMP socket available (check net.ipv4.ping_group_range or grant CAP_NET_RAW): %w", errors.Join(errs...))
	}
	return p, nil
}

func (p *Pinger) needV6() bool {
	for _, ps := range p.peers {
		if ps.ip.Is6() && !ps.ip.Is4In6() {
			return true
		}
		// Unresolved hostnames might resolve to v6 later.
		if !ps.ip.IsValid() {
			if _, err := netip.ParseAddr(ps.Addr); err != nil {
				return true
			}
		}
	}
	return false
}

func openFamily(v6 bool) (*family, error) {
	f := &family{echoType: ipv4.ICMPTypeEcho, replyTyp: ipv4.ICMPTypeEchoReply, proto: protoICMP}
	dgramNet, rawNet, addr := "udp4", "ip4:icmp", "0.0.0.0"
	if v6 {
		f.echoType, f.replyTyp, f.proto = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply, protoICMPv6
		dgramNet, rawNet, addr = "udp6", "ip6:ipv6-icmp", "::"
	}
	c, err := icmp.ListenPacket(dgramNet, addr)
	if err == nil {
		f.conn, f.dgram = c, true
		return f, nil
	}
	c, err2 := icmp.ListenPacket(rawNet, addr)
	if err2 != nil {
		return nil, fmt.Errorf("%s: %v; %s: %v", dgramNet, err, rawNet, err2)
	}
	f.conn = c
	return f, nil
}

// Run sends probes until ctx is done.
func (p *Pinger) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, f := range []*family{p.v4, p.v6} {
		if f == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.receive(f)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(resolveCheck)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				p.maybeResolve(ctx, now)
			}
		}
	}()

	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		p.sendAll()
		select {
		case <-ctx.Done():
			for _, f := range []*family{p.v4, p.v6} {
				if f != nil {
					f.conn.Close()
				}
			}
			wg.Wait()
			return
		case <-t.C:
		}
	}
}

// maybeResolve (re)resolves peers that are due. DNS can block for seconds, so
// lookups run without the lock, and Run calls this from its own goroutine so
// a slow resolver never delays probes to other peers.
func (p *Pinger) maybeResolve(ctx context.Context, now time.Time) {
	type job struct {
		ps   *peerState
		addr string
		old  netip.Addr
	}
	var jobs []job
	p.mu.Lock()
	for _, ps := range p.peers {
		wait := resolveEvery
		if !ps.ip.IsValid() {
			wait = resolveRetry
		}
		if ps.resolvedAt.IsZero() || now.Sub(ps.resolvedAt) >= wait {
			jobs = append(jobs, job{ps, ps.Addr, ps.ip})
		}
	}
	p.mu.Unlock()

	for _, j := range jobs {
		ip, err := resolveFn(ctx, j.addr)
		if err != nil {
			p.log.Warn("ping: resolve failed", "peer", j.ps.Name, "addr", j.addr, "err", err)
		} else if j.old.IsValid() && ip != j.old {
			p.log.Info("ping: peer address changed", "peer", j.ps.Name, "from", j.old, "to", ip)
		}
		p.mu.Lock()
		j.ps.resolvedAt = now
		if err == nil {
			j.ps.ip = ip
		}
		p.mu.Unlock()
	}
}

// resolveFn is swapped out in tests.
var resolveFn = resolve

// resolve prefers IPv4 for hostnames.
func resolve(ctx context.Context, addr string) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(addr); err == nil {
		return ip.Unmap(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", addr)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, ip := range ips {
		if ip.Unmap().Is4() {
			return ip.Unmap(), nil
		}
	}
	if len(ips) == 0 {
		return netip.Addr{}, errors.New("no addresses")
	}
	return ips[0], nil
}

func (p *Pinger) sendAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ps := range p.peers {
		if !ps.ip.IsValid() {
			continue
		}
		f := p.v4
		if !ps.ip.Is4() {
			f = p.v6
		}
		if f == nil {
			continue
		}
		p.seq++
		seq := p.seq
		msg := icmp.Message{
			Type: f.echoType,
			Body: &icmp.Echo{ID: p.id, Seq: int(seq), Data: make([]byte, payloadSize)},
		}
		b, err := msg.Marshal(nil)
		if err != nil {
			continue
		}
		var dst net.Addr = &net.IPAddr{IP: ps.ip.AsSlice()}
		if f.dgram {
			dst = &net.UDPAddr{IP: ps.ip.AsSlice()}
		}
		pr := &probe{ip: ps.ip, seq: seq, sent: time.Now()}
		// Record before sending so a fast reply can't race the bookkeeping.
		key := inflightKey{ps.ip, seq}
		p.inflight[key] = pr // replaces a stale probe if seq wrapped
		ps.probes = append(ps.probes, pr)
		if _, err := f.conn.WriteTo(b, dst); err != nil {
			// Count as sent-and-lost: the path is broken from our side.
			p.log.Debug("ping: send failed", "peer", ps.Name, "err", err)
		}
	}
}

func (p *Pinger) receive(f *family) {
	buf := make([]byte, 1500)
	for {
		n, from, err := f.conn.ReadFrom(buf)
		now := time.Now()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			p.log.Debug("ping: read", "err", err)
			continue
		}
		msg, err := icmp.ParseMessage(f.proto, buf[:n])
		if err != nil || msg.Type != f.replyTyp {
			continue
		}
		echo, ok := msg.Body.(*icmp.Echo)
		if !ok {
			continue
		}
		if !f.dgram && echo.ID != p.id {
			continue // raw sockets see every process's replies
		}
		var src netip.Addr
		switch a := from.(type) {
		case *net.UDPAddr:
			src, _ = netip.AddrFromSlice(a.IP)
		case *net.IPAddr:
			src, _ = netip.AddrFromSlice(a.IP)
		}
		key := inflightKey{src.Unmap(), uint16(echo.Seq)}
		p.mu.Lock()
		if pr := p.inflight[key]; pr != nil {
			pr.got, pr.rtt = true, now.Sub(pr.sent)
			delete(p.inflight, key)
		}
		p.mu.Unlock()
	}
}

// Snapshot returns stats for probes that are complete at now: replied, or
// older than the timeout. Pending probes carry over to the next snapshot.
func (p *Pinger) Snapshot(now time.Time) []Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Stats, 0, len(p.peers))
	for _, ps := range p.peers {
		st := Stats{Target: ps.Name}
		if ps.ip.IsValid() {
			st.Addr = ps.ip.String()
		}
		var rtts []float64
		keep := ps.probes[:0]
		for _, pr := range ps.probes {
			switch {
			case pr.got:
				st.Sent++
				rtts = append(rtts, float64(pr.rtt)/float64(time.Millisecond))
			case now.Sub(pr.sent) >= p.timeout:
				st.Sent++
				st.Lost++
				if p.inflight[inflightKey{pr.ip, pr.seq}] == pr {
					delete(p.inflight, inflightKey{pr.ip, pr.seq})
				}
			default:
				keep = append(keep, pr)
			}
		}
		clear(ps.probes[len(keep):])
		ps.probes = keep
		summarize(&st, rtts)
		out = append(out, st)
	}
	return out
}

func summarize(st *Stats, rtts []float64) {
	if len(rtts) == 0 {
		return
	}
	st.Min, st.Max = math.Inf(1), math.Inf(-1)
	var sum, jit float64
	for i, r := range rtts {
		st.Min = min(st.Min, r)
		st.Max = max(st.Max, r)
		sum += r
		if i > 0 {
			jit += math.Abs(r - rtts[i-1])
		}
	}
	st.Avg = sum / float64(len(rtts))
	if len(rtts) > 1 {
		st.Jitter = jit / float64(len(rtts)-1)
	}
}

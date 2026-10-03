// Package ping measures latency to a fixed set of peers. A peer is probed
// with ICMP echo, or over UDP for paths that only carry TCP/UDP, such as a
// port forward: a DNS query when the far end is a resolver (e.g. 1.1.1.1:53),
// or an authenticated request when it is a vps-probe-echo responder.
//
// However a probe travels, it is one numbered request that is answered within
// the timeout or counted as lost. The ways to send one are links (icmp.go,
// udp.go); everything else is the same for all of them.
package ping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/shakespark/vps-probe/internal/peer"
)

const (
	// Interval and Timeout are what the agent runs with.
	Interval = time.Second
	Timeout  = 2 * time.Second

	resolveEvery = 10 * time.Minute // refresh a good address
	resolveRetry = 30 * time.Second // retry after a failed lookup
	resolveCheck = 5 * time.Second
)

// Stats summarizes the probes to one peer that completed in a window.
type Stats struct {
	Target string
	Addr   string // resolved IP (ip:port for UDP peers), empty if unresolved
	Sent   int
	Lost   int
	Min    float64 // milliseconds, over replies only
	Avg    float64
	Max    float64
	Jitter float64 // mean |rtt[i] - rtt[i-1]|
}

// link is one way to reach peers. send transmits probe seq to ip; the link
// reports a reply by calling Pinger.answer.
type link interface {
	send(ip netip.Addr, seq uint16) error
	// addr is how the probed address is reported.
	addr(ip netip.Addr) string
	close()
}

type probe struct {
	peer *target
	ip   netip.Addr
	seq  uint16
	sent time.Time
	rtt  time.Duration
	got  bool
}

type target struct {
	peer.Peer
	host       string // Addr without the port
	link       link   // nil when the peer cannot be probed (no ICMP socket)
	ip         netip.Addr
	resolvedAt time.Time
	probes     []*probe // sent and not yet reported by Snapshot
}

// Pinger sends one probe per peer per interval.
type Pinger struct {
	interval, timeout time.Duration
	log               *slog.Logger

	mu      sync.Mutex
	peers   []*target
	icmp    *icmpLink
	seq     uint16
	waiting map[uint16]*probe // sent, neither answered nor timed out
	readers sync.WaitGroup
}

// New prepares the peers and opens the sockets they need. It fails only if
// no peer at all can be probed.
func New(peers []peer.Peer, interval, timeout time.Duration, log *slog.Logger) (*Pinger, error) {
	p := &Pinger{interval: interval, timeout: timeout, log: log, waiting: map[uint16]*probe{}}
	var needICMP []*target
	for _, pr := range peers {
		if err := pr.Validate(); err != nil {
			return nil, fmt.Errorf("ping: peer %w", err)
		}
		t := &target{Peer: pr, host: pr.Addr}
		if pr.UDP() {
			u, host := newUDPLink(p, t)
			t.link, t.host = u, host
		} else {
			needICMP = append(needICMP, t)
		}
		p.peers = append(p.peers, t)
	}
	p.maybeResolve(context.Background(), time.Now())
	if len(needICMP) == 0 {
		return p, nil
	}
	ic, err := openICMP(p, needICMP, log)
	if err != nil {
		if len(needICMP) == len(peers) {
			return nil, err
		}
		log.Error("ICMP peers disabled, UDP peers still probed", "err", err)
		return p, nil
	}
	p.icmp = ic
	for _, t := range needICMP {
		t.link = ic
	}
	return p, nil
}

// Run sends probes until ctx is done.
func (p *Pinger) Run(ctx context.Context) {
	if p.icmp != nil {
		p.icmp.listen()
	}
	resolved := make(chan struct{})
	go func() {
		defer close(resolved)
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
			p.mu.Lock()
			for _, t := range p.peers {
				if t.link != nil {
					t.link.close()
				}
			}
			p.mu.Unlock()
			<-resolved
			p.readers.Wait()
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
		t    *target
		host string
		old  netip.Addr
	}
	var jobs []job
	p.mu.Lock()
	for _, t := range p.peers {
		wait := resolveEvery
		if !t.ip.IsValid() {
			wait = resolveRetry
		}
		if t.resolvedAt.IsZero() || now.Sub(t.resolvedAt) >= wait {
			jobs = append(jobs, job{t, t.host, t.ip})
		}
	}
	p.mu.Unlock()

	for _, j := range jobs {
		ip, err := resolveFn(ctx, j.host)
		if err != nil {
			p.log.Warn("ping: resolve failed", "peer", j.t.Name, "addr", j.host, "err", err)
		} else if j.old.IsValid() && ip != j.old {
			p.log.Info("ping: peer address changed", "peer", j.t.Name, "from", j.old, "to", ip)
		}
		p.mu.Lock()
		j.t.resolvedAt = now
		if err == nil {
			j.t.ip = ip
		}
		p.mu.Unlock()
	}
}

// resolveFn is swapped out in tests.
var resolveFn = resolve

// resolve prefers IPv4 for hostnames.
func resolve(ctx context.Context, host string) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap(), nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
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
	for _, t := range p.peers {
		if t.link == nil || !t.ip.IsValid() {
			continue
		}
		p.seq++
		pr := &probe{peer: t, ip: t.ip, seq: p.seq, sent: time.Now()}
		// Recorded before sending, so a fast reply can't race the bookkeeping.
		p.waiting[pr.seq] = pr // replaces a stale probe if seq wrapped
		t.probes = append(t.probes, pr)
		if err := t.link.send(t.ip, pr.seq); err != nil {
			// Counted as sent and lost: the path is broken from our side.
			p.log.Debug("ping: send failed", "peer", t.Name, "err", err)
		}
	}
}

// answer records a reply to probe seq that arrived at now, if from accepts
// the probe as the one this reply belongs to. A reply later than the timeout
// leaves the probe unanswered, and Snapshot counts it as lost: otherwise a
// probe pending at one snapshot could be answered any time before the next
// and be reported as a 10-second round trip.
func (p *Pinger) answer(seq uint16, now time.Time, from func(*probe) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := p.waiting[seq]
	if pr == nil || !from(pr) {
		return
	}
	delete(p.waiting, seq)
	if rtt := now.Sub(pr.sent); rtt < p.timeout {
		pr.got, pr.rtt = true, rtt
	}
}

// Snapshot returns stats for probes that are complete at now: replied, or
// older than the timeout. Pending probes carry over to the next snapshot.
func (p *Pinger) Snapshot(now time.Time) []Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Stats, 0, len(p.peers))
	for _, t := range p.peers {
		st := Stats{Target: t.Name}
		if t.ip.IsValid() {
			st.Addr = t.ip.String()
			if t.link != nil {
				st.Addr = t.link.addr(t.ip)
			}
		}
		var rtts []float64
		keep := t.probes[:0]
		for _, pr := range t.probes {
			switch {
			case pr.got:
				st.Sent++
				rtts = append(rtts, float64(pr.rtt)/float64(time.Millisecond))
			case now.Sub(pr.sent) >= p.timeout:
				st.Sent++
				st.Lost++
				if p.waiting[pr.seq] == pr {
					delete(p.waiting, pr.seq)
				}
			default:
				keep = append(keep, pr)
			}
		}
		clear(t.probes[len(keep):])
		t.probes = keep
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

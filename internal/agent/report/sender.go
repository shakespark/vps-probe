package report

import (
	"context"
	"crypto/cipher"
	"errors"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	pb "github.com/shakespark/vps-probe/internal/proto/probev1"
	"github.com/shakespark/vps-probe/internal/wire"
)

const (
	// An hour of reports are kept for retry; older ones are dropped first,
	// and any report is given up on after wire.MaxAge.
	queueCap      = int(time.Hour / wire.Interval)
	retryAfter    = 5 * time.Second
	aliveWindow   = 30 * time.Second
	drainPerTick  = 10 // per pumpEvery, i.e. 20/s
	pumpEvery     = 500 * time.Millisecond
	redialEvery   = 60 * time.Second
	silenceWarn   = 2 * time.Minute
	warnRepeat    = 10 * time.Minute
	readBufferLen = 2048
)

// Sender delivers packets over a connected UDP socket. Connecting means the
// kernel drops datagrams from anyone but the server, so the agent accepts
// nothing from the internet at large. The only thing read back is an Ack,
// which merely removes delivered reports from the retry queue.
type Sender struct {
	addr string
	node string
	aead cipher.AEAD
	log  *slog.Logger
	now  func() time.Time

	mu       sync.Mutex
	conn     *net.UDPConn
	queue    []*item // oldest first
	started  time.Time
	lastAck  time.Time
	lastDial time.Time
	lastWarn time.Time
	acked    uint64
}

type item struct {
	Packet
	lastSent time.Time
}

func NewSender(addr, node, token string, log *slog.Logger) (*Sender, error) {
	aead, err := wire.NewAEAD(token, node)
	if err != nil {
		return nil, err
	}
	return &Sender{addr: addr, node: node, aead: aead, log: log, now: time.Now}, nil
}

// Enqueue packetizes rep and sends it right away; unacknowledged packets are
// retried by Run.
func (s *Sender) Enqueue(rep *pb.Report) {
	pkts, err := Packetize(s.aead, s.node, rep)
	if err != nil {
		s.log.Error("report: packetize", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, p := range pkts {
		it := &item{Packet: p}
		s.queue = append(s.queue, it)
		s.sendLocked(it, now)
	}
	s.trimLocked(now)
}

// Run owns the socket until ctx is done.
func (s *Sender) Run(ctx context.Context) {
	s.mu.Lock()
	s.started = s.now()
	s.mu.Unlock()
	s.redial()

	t := time.NewTicker(pumpEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			conn := s.conn
			s.conn = nil
			s.mu.Unlock()
			if conn != nil {
				conn.Close()
			}
			return
		case <-t.C:
			if s.pump() {
				s.redial()
			}
		}
	}
}

// redial resolves and connects without holding the lock, so a slow DNS
// lookup never blocks Enqueue or ack handling, then swaps the socket in.
func (s *Sender) redial() {
	s.mu.Lock()
	s.lastDial = s.now()
	s.mu.Unlock()

	var conn *net.UDPConn
	raddr, err := net.ResolveUDPAddr("udp", s.addr)
	if err != nil {
		s.log.Warn("report: resolve server", "addr", s.addr, "err", err)
	} else if conn, err = net.DialUDP("udp", nil, raddr); err != nil {
		s.log.Warn("report: dial server", "addr", raddr, "err", err)
	}

	s.mu.Lock()
	old := s.conn
	s.conn = conn // nil on failure; sends are skipped until the next redial
	s.mu.Unlock()
	if old != nil {
		old.Close() // its reader goroutine exits on the close error
	}
	if conn != nil {
		go s.read(conn)
	}
}

func (s *Sender) read(conn *net.UDPConn) {
	buf := make([]byte, readBufferLen)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// ECONNREFUSED etc. from ICMP errors on a connected socket.
			s.log.Debug("report: read", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.handleAck(buf[:n])
	}
}

func (s *Sender) handleAck(pkt []byte) {
	h, err := wire.ParseHeader(pkt)
	if err != nil || h.Type != wire.TypeAck || h.Node != s.node {
		return
	}
	pt, err := h.Open(s.aead)
	if err != nil {
		return
	}
	var ack pb.Ack
	if proto.Unmarshal(pt, &ack) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastAck.IsZero() {
		s.log.Info("report: server acknowledged, connection established", "addr", s.addr)
	} else if s.now().Sub(s.lastAck) >= silenceWarn {
		s.log.Info("report: server reachable again", "addr", s.addr)
	}
	s.lastAck = s.now()
	s.queue = slices.DeleteFunc(s.queue, func(it *item) bool {
		if slices.Contains(ack.Ids, it.ID) {
			s.acked++
			return true
		}
		return false
	})
}

// pump retries due packets and reports whether the socket should be redialed.
func (s *Sender) pump() (redial bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.trimLocked(now)

	alive := !s.lastAck.IsZero() && now.Sub(s.lastAck) < aliveWindow
	budget := drainPerTick
	if !alive {
		budget = 1 // probe with the newest report only
	}
	for i := len(s.queue) - 1; i >= 0 && budget > 0; i-- {
		it := s.queue[i]
		if now.Sub(it.lastSent) >= retryAfter {
			s.sendLocked(it, now)
			budget--
		}
	}

	// Redialing picks up DNS changes for hostname addresses.
	redial = !alive && now.Sub(s.lastDial) >= redialEvery

	since := s.lastAck
	if since.IsZero() {
		since = s.started
	}
	if now.Sub(since) >= silenceWarn && now.Sub(s.lastWarn) >= warnRepeat {
		s.lastWarn = now
		s.log.Warn("report: no acknowledgement from server; the server drops invalid packets silently, so check: "+
			"server running and listening on this UDP port; firewall / cloud security group allows it; "+
			"node id and token match the server config exactly; system clocks within 2h",
			"addr", s.addr, "node", s.node, "silent_for", now.Sub(since).Round(time.Second), "queued", len(s.queue))
	}
	return redial
}

func (s *Sender) sendLocked(it *item, now time.Time) {
	if s.conn == nil {
		return // not sent: the next pump after a dial picks it up, not one retryAfter later
	}
	it.lastSent = now
	if _, err := s.conn.Write(it.Bytes); err != nil {
		s.log.Debug("report: send", "err", err)
	}
}

func (s *Sender) trimLocked(now time.Time) {
	cut := now.Add(-wire.MaxAge).Unix()
	s.queue = slices.DeleteFunc(s.queue, func(it *item) bool { return it.TS < cut })
	if over := len(s.queue) - queueCap; over > 0 {
		clear(s.queue[:over])
		s.queue = s.queue[over:]
	}
}

// Stats is for diagnostics and tests.
func (s *Sender) Stats() (queued int, acked uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue), s.acked
}

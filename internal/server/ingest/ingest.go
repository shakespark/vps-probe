// Package ingest receives agent reports over UDP. Anything that fails a check
// is dropped without a reply, so to a scanner the port looks closed. Only
// authenticated packets are parsed as protobuf, and only those get an ACK,
// which is smaller than the report so it can't be used for amplification.
package ingest

import (
	"context"
	"crypto/cipher"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	pb "vpsprobe/internal/proto/probev1"
	"vpsprobe/internal/server/config"
	"vpsprobe/internal/wire"
)

const (
	// MaxSkew bounds |server time - report ts|. The agent gives up on
	// reports older than 2h, so anything beyond that is a replay or a
	// badly wrong clock.
	MaxSkew = 2*time.Hour + 5*time.Minute

	seenPruneEvery = time.Minute
	logEvery       = time.Minute
	readBuffer     = 2048 // > wire.MaxPacket, so oversized packets are seen as such
)

// Drop and outcome counters, reported by the API.
const (
	Accepted      = "accepted"
	Duplicate     = "duplicate" // already stored; ACKed again
	Malformed     = "malformed"
	UnknownNode   = "unknown_node"
	AuthFailed    = "auth_failed"
	WrongType     = "wrong_type"
	DecodeFailed  = "decode_failed"
	TSOutOfRange  = "ts_out_of_range"
	StoreFailed   = "store_failed"
	FieldsDropped = "fields_dropped" // report accepted, some values out of range
)

var counterNames = []string{Accepted, Duplicate, Malformed, UnknownNode, AuthFailed, WrongType,
	DecodeFailed, TSOutOfRange, StoreFailed, FieldsDropped}

// Writer persists an accepted report.
type Writer interface {
	Write(node string, rep *pb.Report, from netip.Addr, arrival time.Time) error
}

type Server struct {
	conn  *net.UDPConn
	store Writer
	log   *slog.Logger
	now   func() time.Time
	nodes map[string]cipher.AEAD

	counters map[string]*atomic.Uint64

	// Touched only by the read loop.
	seen      map[string]map[uint64]int64 // node -> report id -> ts
	lastPrune time.Time

	logMu   sync.Mutex
	lastLog map[string]time.Time
}

// Listen binds the UDP port. Keys are derived once here.
func Listen(addr string, nodes []config.Node, store Writer, log *slog.Logger) (*Server, error) {
	s := &Server{
		store:    store,
		log:      log,
		now:      time.Now,
		nodes:    make(map[string]cipher.AEAD, len(nodes)),
		counters: make(map[string]*atomic.Uint64, len(counterNames)),
		seen:     map[string]map[uint64]int64{},
		lastLog:  map[string]time.Time{},
	}
	for _, n := range counterNames {
		s.counters[n] = new(atomic.Uint64)
	}
	for _, n := range nodes {
		aead, err := wire.NewAEAD(n.Token, n.ID)
		if err != nil {
			return nil, err
		}
		s.nodes[n.ID] = aead
	}
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	if s.conn, err = net.ListenUDP("udp", ua); err != nil {
		return nil, err
	}
	s.conn.SetReadBuffer(1 << 20)
	return s, nil
}

func (s *Server) Addr() net.Addr { return s.conn.LocalAddr() }

// Stats returns a snapshot of the counters.
func (s *Server) Stats() map[string]uint64 {
	out := make(map[string]uint64, len(s.counters))
	for k, v := range s.counters {
		out[k] = v.Load()
	}
	return out
}

// Run reads packets until ctx is done. Packets are handled one at a time on
// this goroutine, which also makes it the only writer of report data.
func (s *Server) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		s.conn.Close()
	}()
	buf := make([]byte, readBuffer)
	for {
		n, from, err := s.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Warn("ingest: read", "err", err)
			time.Sleep(10 * time.Millisecond)
			continue
		}
		s.handle(buf[:n], from)
	}
}

func (s *Server) count(name string) { s.counters[name].Add(1) }

func (s *Server) handle(pkt []byte, from netip.AddrPort) {
	now := s.now()
	h, err := wire.ParseHeader(pkt)
	if err != nil {
		s.count(Malformed)
		return
	}
	aead, ok := s.nodes[h.Node]
	if !ok {
		s.count(UnknownNode)
		s.rateLog("unknown", "ingest: packet for a node not in the config", "node", h.Node, "from", from.Addr())
		return
	}
	pt, err := h.Open(aead)
	if err != nil {
		s.count(AuthFailed)
		s.rateLog("auth:"+h.Node, "ingest: authentication failed; token mismatch?", "node", h.Node, "from", from.Addr())
		return
	}
	// Authenticated from here on; the header was part of the AEAD input.
	if h.Type != wire.TypeReport {
		s.count(WrongType)
		return
	}
	rep := &pb.Report{}
	if err := proto.Unmarshal(pt, rep); err != nil {
		s.count(DecodeFailed)
		s.rateLog("decode:"+h.Node, "ingest: undecodable report; agent/server version mismatch?", "node", h.Node, "err", err)
		return
	}
	if skew := now.Sub(time.Unix(rep.Ts, 0)); skew > MaxSkew || skew < -MaxSkew {
		s.count(TSOutOfRange)
		s.rateLog("ts:"+h.Node, "ingest: report timestamp too far from server time; check clocks (NTP)",
			"node", h.Node, "skew", skew.Round(time.Second))
		return
	}

	s.pruneSeen(now)
	seen := s.seen[h.Node]
	if seen == nil {
		seen = map[uint64]int64{}
		s.seen[h.Node] = seen
	}
	if _, dup := seen[rep.Id]; dup {
		s.count(Duplicate)
		s.ack(aead, h.Node, rep.Id, from)
		return
	}
	if dropped := Sanitize(rep); dropped > 0 {
		s.count(FieldsDropped)
		s.rateLog("fields:"+h.Node, "ingest: dropped out-of-range values", "node", h.Node, "count", dropped)
	}
	if err := s.store.Write(h.Node, rep, from.Addr(), now); err != nil {
		s.count(StoreFailed)
		s.rateLog("store", "ingest: store write failed; not acknowledging so the agent retries", "err", err)
		return
	}
	seen[rep.Id] = rep.Ts
	s.count(Accepted)
	s.ack(aead, h.Node, rep.Id, from)
}

// ack replies from the listening socket: the agent's socket is connected to
// exactly this address and port, and its kernel drops anything else.
func (s *Server) ack(aead cipher.AEAD, node string, id uint64, to netip.AddrPort) {
	body, err := proto.Marshal(&pb.Ack{Ids: []uint64{id}})
	if err != nil {
		return
	}
	pkt, err := wire.Seal(aead, wire.TypeAck, node, body)
	if err != nil {
		return
	}
	if _, err := s.conn.WriteToUDPAddrPort(pkt, to); err != nil {
		s.rateLog("ack", "ingest: sending ack", "to", to, "err", err)
	}
}

func (s *Server) pruneSeen(now time.Time) {
	if now.Sub(s.lastPrune) < seenPruneEvery {
		return
	}
	s.lastPrune = now
	cut := now.Add(-MaxSkew - time.Minute).Unix()
	for node, m := range s.seen {
		for id, ts := range m {
			if ts < cut {
				delete(m, id)
			}
		}
		if len(m) == 0 {
			delete(s.seen, node)
		}
	}
}

// rateLog logs at most once per key per logEvery, so a flood of bad packets
// can't flood the journal. Node names in keys passed wire.ValidNode.
func (s *Server) rateLog(key, msg string, args ...any) {
	s.logMu.Lock()
	now := s.now()
	if now.Sub(s.lastLog[key]) < logEvery {
		s.logMu.Unlock()
		return
	}
	if len(s.lastLog) > 1000 {
		clear(s.lastLog)
	}
	s.lastLog[key] = now
	s.logMu.Unlock()
	s.log.Warn(msg, args...)
}

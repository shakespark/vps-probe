package ping

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/shakespark/vps-probe/internal/echo"
	"github.com/shakespark/vps-probe/internal/peer"
)

// udpLink probes one peer with a UDP request and reply. Its socket is
// connected to the peer, so the kernel drops datagrams from anywhere else.
type udpLink struct {
	p     *Pinger
	owner *target
	port  uint16

	request func(seq uint16) []byte
	reply   func([]byte) (seq uint16, ok bool)

	conn   *net.UDPConn // connected to connIP:port
	connIP netip.Addr
}

// newUDPLink returns the link for a validated DNS or echo peer, and the host
// part of its address.
func newUDPLink(p *Pinger, t *target) (*udpLink, string) {
	host, port, _ := net.SplitHostPort(t.Addr)
	n, _ := strconv.ParseUint(port, 10, 16)
	l := &udpLink{p: p, owner: t, port: uint16(n), request: dnsQuery, reply: dnsReply}
	if t.Type == peer.Echo {
		key := []byte(t.Key)
		l.request = func(seq uint16) []byte { return echo.Request(key, seq, time.Now()) }
		l.reply = func(b []byte) (uint16, bool) { return echo.ParseReply(key, b) }
	}
	return l, host
}

func (l *udpLink) addr(ip netip.Addr) string { return netip.AddrPortFrom(ip, l.port).String() }

// send is called with Pinger.mu held. The socket is (re)connected when the
// peer's address changes.
func (l *udpLink) send(ip netip.Addr, seq uint16) error {
	if l.conn == nil || l.connIP != ip {
		l.close()
		c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, l.port)))
		if err != nil {
			return err
		}
		l.conn, l.connIP = c, ip
		l.p.readers.Add(1)
		go l.receive(c)
	}
	_, err := l.conn.Write(l.request(seq))
	return err
}

func (l *udpLink) receive(c *net.UDPConn) {
	defer l.p.readers.Done()
	buf := make([]byte, 1500)
	for {
		n, err := c.Read(buf)
		now := time.Now()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// e.g. ECONNREFUSED after an ICMP port unreachable; the probe
			// times out on its own.
			l.p.log.Debug("ping: udp read", "peer", l.owner.Name, "err", err)
			continue
		}
		if seq, ok := l.reply(buf[:n]); ok {
			l.p.answer(seq, now, func(pr *probe) bool { return pr.peer == l.owner })
		}
	}
}

func (l *udpLink) close() {
	if l.conn != nil {
		l.conn.Close() // its receiver exits on the close error
		l.conn = nil
	}
}

// dnsQuery asks for one.one.one.one A: 1.1.1.1 is authoritative for it and
// any resolver has it cached, so the answer times the path, not the
// resolver. (The root SOA, tried first, had 40ms cache-miss spikes.)
func dnsQuery(id uint16) []byte {
	q := []byte{
		byte(id >> 8), byte(id), // ID
		0x01, 0x00, // flags: RD
		0, 1, 0, 0, 0, 0, 0, 0, // QDCOUNT 1, no other sections
	}
	for range 4 {
		q = append(q, 3, 'o', 'n', 'e')
	}
	return append(q, 0, 0, 1, 0, 1) // root label, QTYPE A, QCLASS IN
}

// dnsReply accepts any response, whatever its rcode: an answer came back
// over the path.
func dnsReply(b []byte) (uint16, bool) {
	if len(b) < 12 || b[2]&0x80 == 0 { // too short, or not a response
		return 0, false
	}
	return binary.BigEndian.Uint16(b), true
}

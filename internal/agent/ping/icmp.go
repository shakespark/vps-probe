package ping

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	protoICMP   = 1
	protoICMPv6 = 58
	payloadSize = 16
)

// icmpLink sends ICMP echo requests over one socket per address family,
// shared by every ICMP peer.
//
// It prefers unprivileged datagram sockets (net.ipv4.ping_group_range) and
// falls back to raw sockets, which need CAP_NET_RAW. With datagram sockets
// the kernel rewrites the echo ID and filters replies per socket, so replies
// are matched on sequence number and source address only.
type icmpLink struct {
	p      *Pinger
	id     int
	v4, v6 *family
}

type family struct {
	conn      *icmp.PacketConn
	dgram     bool
	echoType  icmp.Type
	replyType icmp.Type
	proto     int
}

// openICMP opens the IPv4 socket, and the IPv6 one if a peer may need it.
func openICMP(p *Pinger, peers []*target, log *slog.Logger) (*icmpLink, error) {
	l := &icmpLink{p: p, id: os.Getpid() & 0xffff}
	var errs []error
	if f, err := openFamily(false); err == nil {
		l.v4 = f
		log.Info("ping: ipv4 socket open", "unprivileged", f.dgram)
	} else {
		errs = append(errs, err)
	}
	if needV6(peers) {
		if f, err := openFamily(true); err == nil {
			l.v6 = f
			log.Info("ping: ipv6 socket open", "unprivileged", f.dgram)
		} else {
			errs = append(errs, err)
		}
	}
	if l.v4 == nil && l.v6 == nil {
		return nil, fmt.Errorf("ping: no ICMP socket available (check net.ipv4.ping_group_range or grant CAP_NET_RAW): %w", errors.Join(errs...))
	}
	return l, nil
}

func needV6(peers []*target) bool {
	for _, t := range peers {
		if t.ip.Is6() && !t.ip.Is4In6() {
			return true
		}
		// An unresolved host name might resolve to IPv6 later.
		if !t.ip.IsValid() {
			if _, err := netip.ParseAddr(t.host); err != nil {
				return true
			}
		}
	}
	return false
}

func openFamily(v6 bool) (*family, error) {
	f := &family{echoType: ipv4.ICMPTypeEcho, replyType: ipv4.ICMPTypeEchoReply, proto: protoICMP}
	dgramNet, rawNet, addr := "udp4", "ip4:icmp", "0.0.0.0"
	if v6 {
		f.echoType, f.replyType, f.proto = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply, protoICMPv6
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

func (l *icmpLink) families() []*family {
	var out []*family
	for _, f := range []*family{l.v4, l.v6} {
		if f != nil {
			out = append(out, f)
		}
	}
	return out
}

func (l *icmpLink) listen() {
	for _, f := range l.families() {
		l.p.readers.Add(1)
		go l.receive(f)
	}
}

func (l *icmpLink) addr(ip netip.Addr) string { return ip.String() }

func (l *icmpLink) send(ip netip.Addr, seq uint16) error {
	f := l.v4
	if !ip.Is4() {
		f = l.v6
	}
	if f == nil {
		return errors.New("no socket for this address family")
	}
	msg := icmp.Message{
		Type: f.echoType,
		Body: &icmp.Echo{ID: l.id, Seq: int(seq), Data: make([]byte, payloadSize)},
	}
	b, err := msg.Marshal(nil)
	if err != nil {
		return err
	}
	var dst net.Addr = &net.IPAddr{IP: ip.AsSlice()}
	if f.dgram {
		dst = &net.UDPAddr{IP: ip.AsSlice()}
	}
	_, err = f.conn.WriteTo(b, dst)
	return err
}

func (l *icmpLink) receive(f *family) {
	defer l.p.readers.Done()
	buf := make([]byte, 1500)
	for {
		n, from, err := f.conn.ReadFrom(buf)
		now := time.Now()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			l.p.log.Debug("ping: read", "err", err)
			continue
		}
		msg, err := icmp.ParseMessage(f.proto, buf[:n])
		if err != nil || msg.Type != f.replyType {
			continue
		}
		echo, ok := msg.Body.(*icmp.Echo)
		if !ok {
			continue
		}
		if !f.dgram && echo.ID != l.id {
			continue // raw sockets see every process's replies
		}
		var src netip.Addr
		switch a := from.(type) {
		case *net.UDPAddr:
			src, _ = netip.AddrFromSlice(a.IP)
		case *net.IPAddr:
			src, _ = netip.AddrFromSlice(a.IP)
		}
		src = src.Unmap()
		l.p.answer(uint16(echo.Seq), now, func(pr *probe) bool { return pr.peer.link == l && pr.ip == src })
	}
}

// close is called once per peer using the link; closing twice is harmless.
func (l *icmpLink) close() {
	for _, f := range l.families() {
		f.conn.Close()
	}
}

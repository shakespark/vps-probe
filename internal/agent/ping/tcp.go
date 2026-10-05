package ping

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/shakespark/vps-probe/internal/echo"
)

const (
	tcpDialTimeout = 5 * time.Second
	// A request is 31 bytes into an empty send buffer; a write that takes
	// this long means the connection is stuck.
	tcpWriteTimeout = 100 * time.Millisecond
	// tcpSilence is how long a connection may go without a reply before it
	// is replaced. Without this a path that died silently would be retried
	// by the kernel for many minutes, with every probe lost meanwhile.
	tcpSilence = 10 * time.Second
)

var errNotConnected = errors.New("not connected")

// tcpLink probes one vps-probe-echo responder over a TCP connection that is
// kept open: a request is written every interval and the replies are read
// back. Connecting is not part of any round trip, so the time measured is the
// whole path, not the handshake with a forwarder nearby. A lost segment
// shows up as late replies, which count as lost like any other.
type tcpLink struct {
	p     *Pinger
	owner *target
	port  uint16
	key   []byte

	// Guarded by Pinger.mu.
	conn    net.Conn // nil while not connected
	connIP  netip.Addr
	dialing bool
	cancel  context.CancelFunc // stops the dial in progress

	alive atomic.Int64 // unix nanos of the connect or the latest reply
}

// newTCPLink returns the link for a validated echo-tcp peer, and the host
// part of its address.
func newTCPLink(p *Pinger, t *target) (*tcpLink, string) {
	host, port, _ := net.SplitHostPort(t.Addr)
	n, _ := strconv.ParseUint(port, 10, 16)
	return &tcpLink{p: p, owner: t, port: uint16(n), key: []byte(t.Key)}, host
}

func (l *tcpLink) addr(ip netip.Addr) string { return netip.AddrPortFrom(ip, l.port).String() }

// send is called with Pinger.mu held, so it never waits: while there is no
// connection it starts one in the background and the probe is lost.
func (l *tcpLink) send(ip netip.Addr, seq uint16) error {
	now := time.Now()
	if l.connIP != ip || (l.conn != nil && now.Sub(time.Unix(0, l.alive.Load())) > tcpSilence) {
		l.close()
	}
	if l.conn == nil {
		if !l.dialing {
			ctx, cancel := context.WithCancel(context.Background())
			l.dialing, l.connIP, l.cancel = true, ip, cancel
			l.p.readers.Add(1)
			go l.run(ctx, ip)
		}
		return errNotConnected
	}
	l.conn.SetWriteDeadline(now.Add(tcpWriteTimeout))
	if _, err := l.conn.Write(echo.Request(l.key, seq, now)); err != nil {
		l.close()
		return err
	}
	return nil
}

// run connects to ip and then reads replies until the connection ends.
func (l *tcpLink) run(ctx context.Context, ip netip.Addr) {
	defer l.p.readers.Done()
	d := net.Dialer{Timeout: tcpDialTimeout}
	c, err := d.DialContext(ctx, "tcp", l.addr(ip))

	l.p.mu.Lock()
	l.dialing = false
	if err == nil && ctx.Err() != nil { // closed while connecting
		err = ctx.Err()
		c.Close()
	}
	if err != nil {
		l.p.mu.Unlock()
		l.p.log.Debug("ping: tcp connect", "peer", l.owner.Name, "err", err)
		return
	}
	l.conn = c
	l.alive.Store(time.Now().UnixNano())
	l.p.mu.Unlock()

	buf := make([]byte, echo.Size)
	for {
		_, err = io.ReadFull(c, buf)
		now := time.Now()
		if err != nil {
			break
		}
		seq, ok := echo.ParseReply(l.key, buf)
		if !ok {
			// Not a responder with our key; the stream can't be trusted.
			err = errors.New("unexpected data")
			break
		}
		l.alive.Store(now.UnixNano())
		l.p.answer(seq, now, func(pr *probe) bool { return pr.peer == l.owner })
	}
	if !errors.Is(err, net.ErrClosed) {
		l.p.log.Debug("ping: tcp read", "peer", l.owner.Name, "err", err)
	}
	c.Close()
	l.p.mu.Lock()
	if l.conn == c {
		l.conn = nil
	}
	l.p.mu.Unlock()
}

// close is called with Pinger.mu held.
func (l *tcpLink) close() {
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
	if l.conn != nil {
		l.conn.Close() // its reader exits on the close error
		l.conn = nil
	}
}

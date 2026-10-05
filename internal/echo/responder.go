package echo

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

const (
	// A TCP prober sends a request every second; a connection silent for
	// this long is closed.
	tcpIdle = 30 * time.Second
	// maxConns caps open TCP connections, so idle ones can't use the
	// process up.
	maxConns = 64
)

// Responder answers valid requests. Over UDP everything else is dropped
// without a reply, so a scanner can't tell the port is open. Over TCP
// (optional) the kernel completes the handshake, so the port is visible, but
// nothing is ever written to a connection except replies to valid requests.
type Responder struct {
	Key   []byte
	Allow []netip.Prefix // source addresses answered; empty = any
	// MaxPPS caps replies per second, so even a replayed request can't
	// turn the responder into much of a reflector. 0 = 1000.
	MaxPPS int
	Log    *slog.Logger

	mu                sync.Mutex // Serve and ServeTCP share what is below
	second            time.Time
	sent              int
	lastClog, lastCap time.Time
}

// answer returns the reply to req from src at now, or nil when there is none
// to send.
func (r *Responder) answer(req []byte, src netip.Addr, now time.Time) []byte {
	if len(r.Allow) > 0 && !slices.ContainsFunc(r.Allow, func(p netip.Prefix) bool { return p.Contains(src) }) {
		return nil
	}
	reply, err := Answer(r.Key, req, now)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		// Only authentic requests are logged, at most once a minute.
		if errors.Is(err, ErrClock) && now.Sub(r.lastClog) >= time.Minute {
			r.lastClog = now
			r.Log.Warn("echo: request timestamp too far from this clock; check NTP on both ends", "from", src)
		}
		return nil
	}
	maxPPS := r.MaxPPS
	if maxPPS <= 0 {
		maxPPS = 1000
	}
	if s := now.Truncate(time.Second); !s.Equal(r.second) {
		r.second, r.sent = s, 0
	}
	if r.sent >= maxPPS {
		if now.Sub(r.lastCap) >= time.Minute {
			r.lastCap = now
			r.Log.Warn("echo: reply rate cap reached, dropping", "max_pps", maxPPS)
		}
		return nil
	}
	r.sent++
	return reply
}

// Serve answers on one UDP socket until conn is closed.
func (r *Responder) Serve(conn net.PacketConn) error {
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			r.Log.Debug("echo: read", "err", err)
			continue
		}
		ua, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		reply := r.answer(buf[:n], ua.AddrPort().Addr().Unmap(), time.Now())
		if reply == nil {
			continue
		}
		if _, err := conn.WriteTo(reply, from); err != nil {
			r.Log.Debug("echo: write", "to", from, "err", err)
		}
	}
}

// ServeTCP answers on TCP connections until ln is closed, then closes the
// connections still open. A connection carries requests back to back, each
// answered in turn; the first one that gets no reply ends it, with nothing
// written.
func (r *Responder) ServeTCP(ln net.Listener) error {
	var (
		mu    sync.Mutex
		conns = map[net.Conn]struct{}{}
		wg    sync.WaitGroup
	)
	defer func() {
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			r.Log.Debug("echo: accept", "err", err)
			time.Sleep(100 * time.Millisecond) // e.g. out of file descriptors
			continue
		}
		mu.Lock()
		full := len(conns) >= maxConns
		if !full {
			conns[c] = struct{}{}
			wg.Add(1)
		}
		mu.Unlock()
		if full {
			c.Close()
			continue
		}
		go func() {
			defer wg.Done()
			r.serveConn(c)
			mu.Lock()
			delete(conns, c)
			mu.Unlock()
			c.Close()
		}()
	}
}

func (r *Responder) serveConn(c net.Conn) {
	ta, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	src := ta.AddrPort().Addr().Unmap()
	buf := make([]byte, Size)
	for {
		c.SetReadDeadline(time.Now().Add(tcpIdle))
		if _, err := io.ReadFull(c, buf); err != nil {
			return
		}
		reply := r.answer(buf, src, time.Now())
		if reply == nil {
			return
		}
		c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write(reply); err != nil {
			return
		}
	}
}

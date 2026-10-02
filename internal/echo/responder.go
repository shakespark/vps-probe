package echo

import (
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"time"
)

// Responder answers valid requests on one UDP socket. Everything else is
// dropped without a reply, so a scanner can't tell the port is open.
type Responder struct {
	Key   []byte
	Allow []netip.Prefix // source addresses answered; empty = any
	// MaxPPS caps replies per second, so even a replayed request can't
	// turn the responder into much of a reflector. 0 = 1000.
	MaxPPS int
	Log    *slog.Logger
}

// Serve answers until conn is closed.
func (r *Responder) Serve(conn net.PacketConn) error {
	maxPPS := r.MaxPPS
	if maxPPS <= 0 {
		maxPPS = 1000
	}
	var (
		buf               = make([]byte, 2048)
		second            time.Time
		sent              int
		lastClog, lastCap time.Time
	)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			r.Log.Debug("echo: read", "err", err)
			continue
		}
		now := time.Now()
		ua, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		src := ua.AddrPort().Addr().Unmap()
		if len(r.Allow) > 0 && !slices.ContainsFunc(r.Allow, func(p netip.Prefix) bool { return p.Contains(src) }) {
			continue
		}
		reply, err := Answer(r.Key, buf[:n], now)
		if err != nil {
			// Only authentic requests are logged, at most once a minute.
			if errors.Is(err, ErrClock) && now.Sub(lastClog) >= time.Minute {
				lastClog = now
				r.Log.Warn("echo: request timestamp too far from this clock; check NTP on both ends", "from", src)
			}
			continue
		}
		if s := now.Truncate(time.Second); !s.Equal(second) {
			second, sent = s, 0
		}
		if sent >= maxPPS {
			if now.Sub(lastCap) >= time.Minute {
				lastCap = now
				r.Log.Warn("echo: reply rate cap reached, dropping", "max_pps", maxPPS)
			}
			continue
		}
		sent++
		if _, err := conn.WriteTo(reply, from); err != nil {
			r.Log.Debug("echo: write", "to", from, "err", err)
		}
	}
}

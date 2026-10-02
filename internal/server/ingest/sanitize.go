package ingest

import (
	"math"
	"time"
	"unicode"

	pb "vpsprobe/internal/proto/probev1"
)

// Limits for values from an authenticated agent. A compromised or buggy
// agent can still send nonsense; these keep it from breaking charts or
// overflowing SQLite's signed integers.
const (
	maxItems   = 64
	maxName    = 64
	maxText    = 256
	maxBytes   = math.MaxInt64
	maxRate    = 1e12 // bytes/s
	maxLoad    = 1e4
	maxRTT     = 60000 // ms
	maxPingCnt = 1e5
	maxCount   = 1e7 // sockets, threads
)

// Sanitize removes out-of-range values from rep in place and returns how
// many were removed. It never rejects the whole report.
func Sanitize(rep *pb.Report) (dropped int) {
	drop := func() { dropped++ }

	if s := rep.Sys; s != nil {
		for _, f := range []*string{&s.Hostname, &s.Os, &s.Kernel, &s.Arch, &s.AgentVersion} {
			if !validText(*f, maxText) {
				*f = ""
				drop()
			}
		}
	}
	if c := rep.Cpu; c != nil && !(pct(c.Usage) && pct(c.Steal)) {
		rep.Cpu = nil
		drop()
	}
	if l := rep.Load; l != nil && !(inRange(l.L1, 0, maxLoad) && inRange(l.L5, 0, maxLoad) && inRange(l.L15, 0, maxLoad)) {
		rep.Load = nil
		drop()
	} else if l != nil && l.Threads > maxCount {
		l.Threads = 0
		drop()
	}
	if k := rep.Sockets; k != nil && !(k.Tcp <= maxCount && k.Udp <= maxCount && k.TcpTw <= maxCount) {
		rep.Sockets = nil
		drop()
	}
	if m := rep.Mem; m != nil && !(m.Total <= maxBytes && m.Used <= m.Total && m.SwapTotal <= maxBytes && m.SwapUsed <= m.SwapTotal) {
		rep.Mem = nil
		drop()
	}

	rep.Disks = filter(rep.Disks, &dropped, func(d *pb.Disk) bool {
		return validName(d.Mount) && d.Total <= maxBytes && d.Used <= d.Total && d.Avail <= d.Total && pct(d.InodePct)
	})
	rep.Net = filter(rep.Net, &dropped, func(n *pb.NetRate) bool {
		return validName(n.Iface) && n.RxRate <= maxRate && n.TxRate <= maxRate
	})
	rep.Traffic = filter(rep.Traffic, &dropped, func(t *pb.IfaceTraffic) bool {
		return validName(t.Iface) && validPeriod(t.Cur) && validPeriod(t.Prev)
	})
	rep.Pings = filter(rep.Pings, &dropped, func(p *pb.Ping) bool {
		if !validName(p.Target) || !validText(p.Addr, maxName) || p.Sent > maxPingCnt || p.Lost > p.Sent {
			return false
		}
		if p.Sent == p.Lost {
			return true // no replies; RTT fields are ignored
		}
		return inRange(p.Min, 0, maxRTT) && inRange(p.Avg, p.Min, maxRTT) && inRange(p.Max, p.Avg, maxRTT) &&
			inRange(p.Jitter, 0, maxRTT)
	})
	return dropped
}

func filter[T any](items []T, dropped *int, ok func(T) bool) []T {
	out := items[:0]
	for i, it := range items {
		if i >= maxItems || !ok(it) {
			*dropped++
			continue
		}
		out = append(out, it)
	}
	return out
}

func validPeriod(p *pb.Period) bool {
	if p == nil {
		return true
	}
	if p.Rx > maxBytes || p.Tx > maxBytes {
		return false
	}
	t, err := time.Parse(time.DateOnly, p.Start)
	return err == nil && t.Format(time.DateOnly) == p.Start
}

func pct(v float32) bool { return inRange(v, 0, 100) }

// inRange also rejects NaN (all comparisons false) and infinities.
func inRange(v, lo, hi float32) bool { return v >= lo && v <= hi }

// validName: interface, mount point or peer name. Non-empty, short, printable.
func validName(s string) bool { return s != "" && validText(s, maxName) }

func validText(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

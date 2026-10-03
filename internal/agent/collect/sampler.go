package collect

import "time"

// Sampler reports what happened between one call and the next: the kernel
// only offers counters that have been running since boot. The first call of
// each method has nothing to compare with and reports no value.
type Sampler struct {
	fs FS

	cpu     CPUTimes
	haveCPU bool

	net   map[string]NetCounter
	netAt time.Time
}

// NewSampler takes the first readings, so the next call already has a
// previous one to compare with.
func NewSampler(fs FS) *Sampler {
	s := &Sampler{fs: fs}
	s.CPU()
	s.Net(time.Now())
	return s
}

// CPU returns the usage since the previous call. ok is false when there is
// nothing to compare with: the first call, or the first after a failed one.
func (s *Sampler) CPU() (pct CPUPct, ok bool, err error) {
	cur, err := s.fs.CPUTimes()
	if err != nil {
		s.haveCPU = false
		return CPUPct{}, false, err
	}
	if s.haveCPU {
		pct, ok = CPUUsage(s.cpu, cur)
	}
	s.cpu, s.haveCPU = cur, true
	return pct, ok, nil
}

// NetRate is an interface's speed in bytes and packets per second.
type NetRate struct{ RX, TX, RXPkts, TXPkts uint64 }

// Net returns every interface's counters as of now, and the rates since the
// previous call for the interfaces whose counters only grew in between.
func (s *Sampler) Net(now time.Time) (counters map[string]NetCounter, rates map[string]NetRate, err error) {
	cur, err := s.fs.NetCounters()
	if err != nil {
		s.net = nil
		return nil, nil, err
	}
	rates = map[string]NetRate{}
	if secs := now.Sub(s.netAt).Seconds(); s.net != nil && secs > 0 {
		per := func(c, p uint64) uint64 { return uint64(float64(c-p) / secs) }
		for name, c := range cur {
			p, seen := s.net[name]
			if !seen || c.RX < p.RX || c.TX < p.TX || c.RXPkts < p.RXPkts || c.TXPkts < p.TXPkts {
				continue // new, or its counters restarted
			}
			rates[name] = NetRate{RX: per(c.RX, p.RX), TX: per(c.TX, p.TX),
				RXPkts: per(c.RXPkts, p.RXPkts), TXPkts: per(c.TXPkts, p.TXPkts)}
		}
	}
	s.net, s.netAt = cur, now
	return cur, rates, nil
}

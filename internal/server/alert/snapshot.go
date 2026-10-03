package alert

import (
	"context"
	"slices"
	"time"

	"github.com/shakespark/vps-probe/internal/server/config"
	"github.com/shakespark/vps-probe/internal/server/store"
)

const (
	// Ping metrics are summed over this window; one report's ten pings are
	// too few to judge a link by.
	pingWindow = time.Minute
	// Rate metrics average this much of the node's newest samples.
	netWindow = 60 // seconds, agent clock
	// The offline alert looks this far back from a node's last report for
	// an inbound surge, the usual sign of a provider null-routing it.
	peakLookback = 5 * 60
)

// snapshot is everything one round looks at, read once at its start.
type snapshot struct {
	status map[string]*store.Status
	links  []store.Link
	net    map[string]rate // online nodes: the last netWindow on average
	peak   map[string]rate // offline nodes: the busiest inbound netWindow before they went silent
}

// rate is a node's network speed, its interfaces together.
type rate struct {
	in, out   float64 // Mbps
	pin, pout float64 // packets/s
}

func (e *Evaluator) snapshot(ctx context.Context, now time.Time) (*snapshot, error) {
	s := &snapshot{status: map[string]*store.Status{}, net: map[string]rate{}, peak: map[string]rate{}}
	// Rates are only read for the rules that use them.
	needNet := slices.ContainsFunc(e.cfg.Alerts, func(r config.Rule) bool { return metrics[r.Metric].rate })
	needPeak := len(e.floodRules()) > 0
	for _, n := range e.cfg.Nodes {
		st, err := e.src.Status(ctx, n.ID)
		if err != nil {
			return nil, err
		}
		s.status[n.ID] = st
		switch online := st.Online(now); {
		case st == nil:
		case online && needNet:
			sums, err := e.src.NetSums(ctx, n.ID, st.MaxTS-netWindow+1, st.MaxTS)
			if err != nil {
				return nil, err
			}
			if len(sums) > 0 {
				s.net[n.ID] = average(sums)
			}
		case !online && needPeak:
			sums, err := e.src.NetSums(ctx, n.ID, st.MaxTS-peakLookback-netWindow+1, st.MaxTS)
			if err != nil {
				return nil, err
			}
			if r, ok := busiestIn(sums); ok {
				s.peak[n.ID] = r
			}
		}
	}
	var err error
	s.links, err = e.src.Matrix(ctx, pingWindow)
	return s, err
}

func average(sums []store.NetSum) rate {
	var r rate
	for _, n := range sums {
		r.in, r.out, r.pin, r.pout = r.in+n.RX, r.out+n.TX, r.pin+n.RXPkts, r.pout+n.TXPkts
	}
	k := float64(len(sums))
	return rate{in: mbps(r.in / k), out: mbps(r.out / k), pin: r.pin / k, pout: r.pout / k}
}

// busiestIn averages every netWindow of samples ending at a sample and
// returns the one with the most inbound traffic. Windows begin at the
// first sample, so the earliest ones may be shorter.
func busiestIn(sums []store.NetSum) (rate, bool) {
	var best rate
	found := false
	for i, end := range sums {
		j := i
		for j > 0 && sums[j-1].TS > end.TS-netWindow {
			j--
		}
		if r := average(sums[j : i+1]); !found || r.in > best.in {
			best, found = r, true
		}
	}
	return best, found
}

func mbps(bytesPerSec float64) float64 { return bytesPerSec * 8 / 1e6 }

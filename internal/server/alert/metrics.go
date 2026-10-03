package alert

import (
	"fmt"
	"time"

	"github.com/shakespark/vps-probe/internal/server/config"
	"github.com/shakespark/vps-probe/internal/server/store"
)

// metric is everything the evaluator knows about one of config.Metrics:
// how to measure it and how to show it.
type metric struct {
	label  string
	format func(v float64) string // a value with its unit
	// observe returns the rule's condition on a node right now: one
	// observation, one per mount point or link, or none when there is no
	// usable data. st is not nil.
	observe func(r *round, rule *config.Rule, n *config.Node, st *store.Status) []obs

	// offline: the metric is about nodes that are not online, so observe is
	// also called for those (st may then be nil).
	offline bool
	// timed: the observation's own condition already includes the rule's
	// `for`, so the alert fires and recovers at once.
	timed bool
	// rate: the metric needs the snapshot's network rates.
	rate bool
	// over is how a recovery message calls the time the alert lasted.
	over string
}

// obs is one observed condition.
type obs struct {
	target   string
	value    float64
	cond     bool
	detail   string    // the value in words, e.g. "CPU 95.2%"
	evidence string    // further lines that support or qualify it
	since    time.Time // when the condition began, if that is known better than "when first seen"
}

var metrics = map[string]metric{
	config.MetricOffline: {label: "离线", format: fmtSilence, observe: observeOffline, offline: true, timed: true, over: "离线约"},

	config.MetricCPU:     gauge("CPU", fmtPct, func(st *store.Status) *float64 { return st.CPU }),
	config.MetricSteal:   gauge("Steal", fmtPct, func(st *store.Status) *float64 { return st.Steal }),
	config.MetricSoftIRQ: gauge("软中断", fmtPct, func(st *store.Status) *float64 { return st.SoftIRQ }),
	config.MetricLoad1:   gauge("负载(1m)", fmtLoad, func(st *store.Status) *float64 { return st.Load1 }),
	config.MetricMem:     gauge("内存", fmtPct, func(st *store.Status) *float64 { return share(st.MemUsed, st.MemTotal) }),
	config.MetricSwap:    gauge("Swap", fmtPct, func(st *store.Status) *float64 { return share(st.SwapUsed, st.SwapTotal) }),

	config.MetricDisk:     {label: "磁盘", format: fmtPct, observe: observeDisks},
	config.MetricPingLoss: {label: "丢包", format: fmtPct, observe: observeLinks},
	config.MetricPingAvg:  {label: "时延", format: fmtMs, observe: observeLinks},

	config.MetricNetIn:  {label: "入站", format: fmtMbps, observe: observeBytes, rate: true},
	config.MetricNetOut: {label: "出站", format: fmtMbps, observe: observeBytes, rate: true},
	config.MetricPPSIn:  {label: "入站包速率", format: fmtPPS, observe: observePackets, rate: true},
	config.MetricPPSOut: {label: "出站包速率", format: fmtPPS, observe: observePackets, rate: true},
}

func init() {
	for name, m := range metrics {
		if m.over == "" {
			m.over = "异常持续约"
			metrics[name] = m
		}
	}
}

// inbound reports whether a rate metric watches the inbound direction.
func inbound(metric string) bool { return metric == config.MetricNetIn || metric == config.MetricPPSIn }

// gauge is a metric with one value per node, read from its status; nil when
// the node doesn't report it.
func gauge(label string, format func(float64) string, value func(*store.Status) *float64) metric {
	return metric{label: label, format: format,
		observe: func(_ *round, rule *config.Rule, _ *config.Node, st *store.Status) []obs {
			v := value(st)
			if v == nil {
				return nil
			}
			return []obs{{value: *v, cond: compare(*v, rule), detail: label + " " + format(*v)}}
		}}
}

// share returns used as a percentage of total, nil when either is unknown.
func share(used, total *int64) *float64 {
	if used == nil || total == nil || *total <= 0 {
		return nil
	}
	v := 100 * float64(*used) / float64(*total)
	return &v
}

func compare(v float64, rule *config.Rule) bool {
	switch rule.Op {
	case ">":
		return v > rule.Threshold
	case ">=":
		return v >= rule.Threshold
	case "<":
		return v < rule.Threshold
	case "<=":
		return v <= rule.Threshold
	}
	return false
}

func observeDisks(_ *round, rule *config.Rule, _ *config.Node, st *store.Status) []obs {
	var out []obs
	for _, d := range st.Disks {
		if d.Total > 0 {
			v := 100 * float64(d.Used) / float64(d.Total)
			out = append(out, obs{target: d.Mount, value: v, cond: compare(v, rule), detail: "磁盘 " + d.Mount + " " + fmtPct(v)})
		}
	}
	return out
}

// observeLinks watches the node's links: its pings to each of its peers.
func observeLinks(r *round, rule *config.Rule, n *config.Node, _ *store.Status) []obs {
	var out []obs
	for _, l := range r.snap.links {
		if l.Src != n.ID || l.Sent == 0 || r.reportedDown(l.Dst) {
			continue
		}
		label := fmt.Sprintf("%s → %s", n.ID, l.Dst)
		if rule.Metric == config.MetricPingLoss {
			out = append(out, obs{target: l.Dst, value: l.LossPct, cond: compare(l.LossPct, rule),
				detail: label + " 丢包 " + fmtPct(l.LossPct)})
		} else if l.Avg != nil {
			out = append(out, obs{target: l.Dst, value: *l.Avg, cond: compare(*l.Avg, rule),
				detail: label + " 时延 " + fmtMs(*l.Avg)})
		}
	}
	return out
}

// reportedDown is true when dst is a node that is offline and an offline
// rule covers it. Every link to a dead node loses all its pings; the
// offline alert already says so, and one alert per peer would only repeat
// it. A target that still reports but can't be pinged is not down here, so
// those links alert.
func (r *round) reportedDown(dst string) bool {
	if _, ok := r.cfg.Node(dst); !ok || r.snap.status[dst].Online(r.now) {
		return false
	}
	for i := range r.cfg.Alerts {
		if rule := &r.cfg.Alerts[i]; rule.Metric == config.MetricOffline && rule.Covers(dst) {
			return true
		}
	}
	return false
}

// observeOffline is true once a node has been silent for the rule's `for`.
// A node that never reported counts from when the server started.
func observeOffline(r *round, rule *config.Rule, n *config.Node, st *store.Status) []obs {
	if r.now.Sub(r.started) < StartupGrace {
		return nil
	}
	last := r.started
	if st != nil {
		last = time.Unix(st.FreshAt, 0)
	}
	silent := r.now.Sub(last)
	o := obs{value: silent.Seconds(), cond: silent >= time.Duration(rule.For), since: last, detail: "已 " + fmtDur(silent) + " 没有上报"}
	switch {
	case st == nil:
		o.detail = "从未上报"
	case !o.cond:
		o.detail = "已恢复上报"
	default:
		if p, ok := r.snap.peak[n.ID]; ok && r.floodBefore(n.ID, p) {
			o.detail += fmt.Sprintf("；停止上报前入站 %s、出站 %s，疑似被攻击后遭商家黑洞", fmtMbps(p.in), fmtMbps(p.out))
		}
	}
	return []obs{o}
}

// floodRules are the rules that recognize an inbound flood: net_in with a
// ratio.
func (e *Evaluator) floodRules() []*config.Rule {
	var out []*config.Rule
	for i := range e.cfg.Alerts {
		if r := &e.cfg.Alerts[i]; r.Metric == config.MetricNetIn && r.Ratio > 0 {
			out = append(out, r)
		}
	}
	return out
}

// floodBefore reports whether p, a node's busiest inbound minute before it
// went silent, meets a flood rule that covers the node.
func (e *Evaluator) floodBefore(id string, p rate) bool {
	for _, r := range e.floodRules() {
		if r.Covers(id) && compare(p.in, r) && p.in >= r.Ratio*p.out {
			return true
		}
	}
	return false
}

// A peer whose pings to a node lose at least this much counts towards the
// evidence in a net_in alert.
const fanInLoss = 20.0

// fanIn counts the nodes pinging id over the last minute and those of
// them losing at least fanInLoss: a flood fills the node's inbound link,
// so every peer sees loss at once, while a bad route affects only some.
func (r *round) fanIn(id string) (lossy, measured int) {
	for _, l := range r.snap.links {
		if l.Dst != id || l.Sent == 0 {
			continue
		}
		measured++
		if l.LossPct >= fanInLoss {
			lossy++
		}
	}
	return lossy, measured
}

// observeBytes watches one direction of a node's traffic in Mbps. With a
// ratio the rule looks for lopsided traffic, a flood in or out, and says so,
// with what else points the same way.
func observeBytes(r *round, rule *config.Rule, n *config.Node, st *store.Status) []obs {
	rt, ok := r.snap.net[n.ID]
	if !ok {
		return nil
	}
	in := inbound(rule.Metric)
	own, other := rt.in, rt.out
	if !in {
		own, other = other, own
	}
	o := obs{value: own, cond: compare(own, rule) && own >= rule.Ratio*other,
		detail: fmt.Sprintf("入站 %s，出站 %s", fmtMbps(rt.in), fmtMbps(rt.out))}
	if rule.Ratio == 0 {
		return []obs{o}
	}
	lossy, measured := r.fanIn(n.ID)
	switch {
	case o.cond:
		what := "疑似被利用对外攻击"
		if in {
			what = "疑似 DDoS"
		}
		o.detail = what + "：" + o.detail
		if other > 0 {
			o.detail += fmt.Sprintf("（%.1f 倍）", own/other)
		}
		if in {
			if measured > 0 {
				o.evidence = fmt.Sprintf("%d 个节点中 %d 个到它丢包 ≥ %.0f%%\n", measured, lossy, fanInLoss)
			}
			o.evidence += packetText(rt, st)
		}
	case in && measured > 0 && lossy*2 >= measured:
		// An inbound-only null-route ends the flood at the NIC while the
		// agent's reports still get out: the alert recovers although
		// nothing can reach the node.
		o.evidence = fmt.Sprintf("入站已回落，但 %d 个节点中 %d 个到它仍丢包 ≥ %.0f%%，可能已被商家黑洞", measured, lossy, fanInLoss)
	}
	return []obs{o}
}

// observePackets watches one direction in packets per second: a flood of
// small packets (SYN, tiny UDP) is the one a byte rate misses.
func observePackets(r *round, rule *config.Rule, n *config.Node, st *store.Status) []obs {
	rt, ok := r.snap.net[n.ID]
	if !ok {
		return nil
	}
	own, other := rt.pin, rt.pout
	if !inbound(rule.Metric) {
		own, other = other, own
	}
	return []obs{{value: own, cond: compare(own, rule) && own >= rule.Ratio*other, detail: packetText(rt, st)}}
}

// packetText describes the packet rates, with the average size of an
// inbound packet, and the softirq time they cost.
func packetText(rt rate, st *store.Status) string {
	s := "入站 " + fmtPPS(rt.pin)
	if rt.pin > 0 {
		s += fmt.Sprintf("（平均 %.0f 字节/包）", rt.in*1e6/8/rt.pin)
	}
	s += "，出站 " + fmtPPS(rt.pout)
	if st.SoftIRQ != nil {
		s += "，软中断 " + fmtPct(*st.SoftIRQ)
	}
	return s
}

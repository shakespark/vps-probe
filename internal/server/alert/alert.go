// Package alert evaluates alert rules against the latest stored data and
// sends notifications on state changes. See DESIGN.md §7.
package alert

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"vpsprobe/internal/agent/traffic"
	"vpsprobe/internal/server/config"
	"vpsprobe/internal/server/notify"
	"vpsprobe/internal/server/store"
)

const (
	EvalEvery = 10 * time.Second
	// Offline isn't judged right after the server starts: every node
	// looks silent until its first report arrives.
	StartupGrace = 2 * time.Minute
	// Ping metrics are summed over this window; one 10s sample of 10
	// pings is too noisy.
	pingWindow = time.Minute
	// net_in / net_out average this much of the node's newest samples.
	netWindow = 60 // seconds, agent clock
	// The offline alert looks this far back from a node's last report for
	// an inbound surge, the usual sign of a provider null-routing it.
	peakLookback = 5 * 60
	// A peer whose pings to a node lose at least this much counts towards
	// the evidence in a net_in alert.
	fanInLoss = 20.0
	// A firing alert recovers only after its condition has been false
	// for min(for, maxDebounce).
	maxDebounce = time.Minute
	// Rounds run on a ticker, so "held for 20s" may measure 19.99s; half a
	// round of tolerance keeps alerts from firing one round late.
	slack = EvalEvery / 2
)

func held(since, now time.Time, d time.Duration) bool { return now.Sub(since)+slack >= d }

const (
	statePending = "pending"
	stateFiring  = "firing"
	stateLevel   = "level" // traffic: highest quota level notified this period; expiry: lowest day level
	stateSeen    = "seen"  // ip_change: the node's last known IP, kept in the target
	stateSent    = "sent"  // period_report: the current period start; weekly_report: the last slot sent
)

// Source is what the evaluator needs from the store.
type Source interface {
	Status(ctx context.Context, node string) (*store.Status, error)
	Matrix(ctx context.Context, window time.Duration) ([]store.Link, error)
	NetSums(ctx context.Context, node string, from, to int64) ([]store.NetSum, error)
	Periods(ctx context.Context, node string, limit int) ([]store.Period, error)
	Daily(ctx context.Context, node, start string) ([]store.DayTraffic, error)
	AlertStates() ([]store.AlertState, error)
	SaveAlerts(put, del []store.AlertState, events []store.AlertEvent) error
	PruneAlertStates(rules []string) error
}

type key struct{ rule, node, target string }

type instance struct {
	state      string
	since      time.Time // condition first true
	notified   time.Time
	clearSince time.Time // firing: condition first false again (debounce)
	value      float64
}

// Active is an alert currently pending or firing, for the API.
type Active struct {
	Rule   string  `json:"rule"`
	Metric string  `json:"metric"`
	Node   string  `json:"node"`
	Target string  `json:"target"`
	State  string  `json:"state"`
	Since  int64   `json:"since"`
	Value  float64 `json:"value"`
}

type Evaluator struct {
	cfg     *config.Config
	src     Source
	notify  notify.Notifier
	log     *slog.Logger
	now     func() time.Time
	started time.Time

	mu   sync.Mutex
	inst map[key]*instance
}

func New(cfg *config.Config, src Source, n notify.Notifier, log *slog.Logger) *Evaluator {
	return &Evaluator{cfg: cfg, src: src, notify: n, log: log, now: time.Now, inst: map[key]*instance{}}
}

// Load restores persisted states and drops those of rules or nodes that
// are no longer configured.
func (e *Evaluator) Load() error {
	e.started = e.now()
	names := make([]string, len(e.cfg.Alerts))
	for i, r := range e.cfg.Alerts {
		names[i] = r.Name
	}
	if err := e.src.PruneAlertStates(names); err != nil {
		return err
	}
	states, err := e.src.AlertStates()
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range states {
		if _, ok := e.cfg.Node(s.Node); !ok && s.Node != "" { // "": weekly_report, not per node
			continue
		}
		e.inst[key{s.Rule, s.Node, s.Target}] = &instance{state: s.State, since: time.Unix(s.Since, 0),
			notified: time.Unix(s.Notified, 0), value: s.Value}
	}
	return nil
}

func (e *Evaluator) Run(ctx context.Context) {
	t := time.NewTicker(EvalEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Tick(ctx); err != nil && ctx.Err() == nil {
				e.log.Error("alert evaluation", "err", err)
			}
		}
	}
}

// Active lists pending and firing alerts, most recent first.
func (e *Evaluator) Active() []Active {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []Active{}
	for k, in := range e.inst {
		if in.state == stateLevel || in.state == stateSeen || in.state == stateSent {
			continue
		}
		r := e.rule(k.rule)
		if r == nil {
			continue
		}
		out = append(out, Active{Rule: k.rule, Metric: r.Metric, Node: k.node, Target: k.target,
			State: in.state, Since: in.since.Unix(), Value: in.value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since > out[j].Since })
	return out
}

func (e *Evaluator) rule(name string) *config.Rule {
	for i := range e.cfg.Alerts {
		if e.cfg.Alerts[i].Name == name {
			return &e.cfg.Alerts[i]
		}
	}
	return nil
}

// obs is one evaluated condition: a rule applied to a node (and target).
type obs struct {
	node, target string
	value        float64
	cond         bool
	detail       string // human-readable current value, e.g. "CPU 95.2%"
}

// rate is a node's network speed summed over its interfaces: in Mbps, and
// in packets/s when pkts (agent >= 0.1.14).
type rate struct {
	in, out   float64
	pin, pout float64
	pkts      bool
}

// packets describes the packet rates, with the average packet size of
// the inbound direction: a flood of small packets (SYN, tiny UDP) is the
// one a byte rate misses. Empty for agents that don't report them.
func (rt rate) packets() string {
	if !rt.pkts {
		return ""
	}
	s := "入站 " + fmtPPS(rt.pin)
	if rt.pin > 0 {
		s += fmt.Sprintf("（平均 %.0f 字节/包）", rt.in*1e6/8/rt.pin)
	}
	return s + "，出站 " + fmtPPS(rt.pout)
}

// packetEvidence is the packet rates and softirq time, as an extra line
// of a net_in alert.
func packetEvidence(rt rate, st *store.Status) string {
	var parts []string
	if p := rt.packets(); p != "" {
		parts = append(parts, p)
	}
	if st != nil && st.SoftIRQ != nil {
		parts = append(parts, fmt.Sprintf("软中断 %.1f%%", *st.SoftIRQ))
	}
	return strings.Join(parts, "，")
}

type snapshot struct {
	status map[string]*store.Status
	links  []store.Link
	net    map[string]rate // online nodes: the last netWindow on average
	peak   map[string]rate // offline nodes: the busiest inbound netWindow before they went silent
}

func (e *Evaluator) snapshot(ctx context.Context, now time.Time) (*snapshot, error) {
	s := &snapshot{status: map[string]*store.Status{}, net: map[string]rate{}, peak: map[string]rate{}}
	var needNet, needPeak bool
	for _, r := range e.cfg.Alerts {
		needNet = needNet || r.Rate()
		needPeak = needPeak || r.Metric == config.MetricNetIn && r.Ratio > 0
	}
	for _, n := range e.cfg.Nodes {
		st, err := e.src.Status(ctx, n.ID)
		if err != nil {
			return nil, err
		}
		s.status[n.ID] = st
		if st == nil {
			continue
		}
		online := now.Sub(time.Unix(st.FreshAt, 0)) <= time.Duration(e.cfg.OfflineAfter)
		switch {
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
	var rx, tx, prx, ptx float64
	var withPkts int
	for _, n := range sums {
		rx, tx = rx+n.RX, tx+n.TX
		if n.RXPkts.Valid {
			prx, ptx, withPkts = prx+n.RXPkts.Float64, ptx+n.TXPkts.Float64, withPkts+1
		}
	}
	k := float64(len(sums))
	r := rate{in: mbps(rx / k), out: mbps(tx / k)}
	if withPkts > 0 {
		r.pin, r.pout, r.pkts = prx/float64(withPkts), ptx/float64(withPkts), true
	}
	return r
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

// Tick runs one evaluation round and sends one merged message for it.
func (e *Evaluator) Tick(ctx context.Context) error {
	now := e.now()
	snap, err := e.snapshot(ctx, now)
	if err != nil {
		return err
	}

	e.mu.Lock()
	var put, del []store.AlertState
	var events []store.AlertEvent
	var msgs []string
	emit := func(k key, event string, value float64, msg string) {
		msgs = append(msgs, msg)
		events = append(events, store.AlertEvent{TS: now.Unix(), Rule: k.rule, Node: k.node, Target: k.target,
			Event: event, Value: value, Message: msg})
	}
	persist := func(k key, in *instance) {
		put = append(put, store.AlertState{Rule: k.rule, Node: k.node, Target: k.target, State: in.state,
			Since: in.since.Unix(), Notified: in.notified.Unix(), Value: in.value})
	}
	// drop forgets the rule's other states for k's node, e.g. an expiry
	// date that has been renewed or an IP that is no longer current.
	drop := func(k key) {
		for o := range e.inst {
			if o.rule == k.rule && o.node == k.node && o.target != k.target {
				delete(e.inst, o)
				del = append(del, store.AlertState{Rule: o.rule, Node: o.node, Target: o.target})
			}
		}
	}

	for i := range e.cfg.Alerts {
		r := &e.cfg.Alerts[i]
		switch r.Metric {
		case config.MetricTraffic:
			e.traffic(r, snap, now, emit, persist)
			continue
		case config.MetricExpiry:
			e.expiry(r, now, emit, persist, drop)
			continue
		case config.MetricIPChange:
			e.ipChange(r, snap, now, emit, persist, drop)
			continue
		case config.MetricPeriod, config.MetricWeekly:
			report := e.periodReport
			if r.Metric == config.MetricWeekly {
				report = e.weeklyReport
			}
			// A failed query leaves nothing recorded, so the next round retries.
			if err := report(ctx, r, snap, now, emit, persist, drop); err != nil {
				e.log.Error("traffic report", "rule", r.Name, "err", err)
			}
			continue
		}
		seen := map[key]bool{}
		for _, o := range e.observe(r, snap, now) {
			k := key{r.Name, o.node, o.target}
			seen[k] = true
			in := e.inst[k]
			switch {
			case o.cond && in == nil:
				in = &instance{state: statePending, since: now, value: o.value}
				e.inst[k] = in
				if r.Metric == config.MetricOffline || r.For == 0 {
					e.fire(r, k, in, o, now, emit, persist)
				}
			case o.cond && in.state == statePending:
				in.value = o.value
				if held(in.since, now, time.Duration(r.For)) {
					e.fire(r, k, in, o, now, emit, persist)
				}
			case o.cond: // firing
				in.value, in.clearSince = o.value, time.Time{}
				if r.Repeat > 0 && held(in.notified, now, time.Duration(r.Repeat)) {
					in.notified = now
					emit(k, "repeat", o.value, e.message("🟠 仍在告警", r, k, o.detail, now,
						"已持续 "+fmtDur(now.Sub(in.since))))
					persist(k, in)
				}
			case in == nil:
			case in.state == statePending:
				delete(e.inst, k)
			default: // firing, condition false
				if in.clearSince.IsZero() {
					in.clearSince = now
				}
				if held(in.clearSince, now, debounce(r)) {
					delete(e.inst, k)
					del = append(del, store.AlertState{Rule: k.rule, Node: k.node, Target: k.target})
					title, detail, extra := "🟢 恢复", o.detail, "异常持续约 "+fmtDur(in.clearSince.Sub(in.since))
					if r.Metric == config.MetricOffline {
						// in.value holds the longest silence seen while firing.
						title, detail, extra = "🟢 恢复在线", "已恢复上报", "离线约 "+fmtDur(time.Duration(in.value)*time.Second)
					}
					msg := e.message(title, r, k, detail, now, extra)
					if r.Recovers() {
						emit(k, "recovered", o.value, msg)
					} else {
						events = append(events, store.AlertEvent{TS: now.Unix(), Rule: k.rule, Node: k.node,
							Target: k.target, Event: "recovered", Value: o.value, Message: msg})
					}
				}
			}
		}
		// No data this round (node offline, link gone, startup grace):
		// pending starts over, firing holds.
		for k, in := range e.inst {
			if k.rule == r.Name && !seen[k] && in.state == statePending {
				delete(e.inst, k)
			}
		}
	}
	e.mu.Unlock()

	if err := e.src.SaveAlerts(put, del, events); err != nil {
		e.log.Error("saving alert state", "err", err)
	}
	if len(msgs) > 0 {
		e.notify.Notify(strings.Join(msgs, "\n\n"))
	}
	return nil
}

func debounce(r *config.Rule) time.Duration {
	if r.Metric == config.MetricOffline {
		return 0 // a fresh report settles it
	}
	return min(time.Duration(r.For), maxDebounce)
}

func (e *Evaluator) fire(r *config.Rule, k key, in *instance, o obs, now time.Time,
	emit func(key, string, float64, string), persist func(key, *instance)) {
	in.state, in.notified = stateFiring, now
	var extra string
	switch {
	case r.Metric == config.MetricOffline:
		extra = ""
	case r.For > 0:
		extra = "已持续 " + fmtDur(now.Sub(in.since))
	}
	emit(k, "firing", o.value, e.message("🔴 告警", r, k, o.detail, now, extra))
	persist(k, in)
}

func (e *Evaluator) nodeName(id string) string {
	if n, ok := e.cfg.Node(id); ok && n.Name != id {
		return fmt.Sprintf("%s（%s）", n.Name, id)
	}
	return id
}

func (e *Evaluator) message(title string, r *config.Rule, k key, detail string, now time.Time, extra string) string {
	var b strings.Builder
	// A second line of detail is supporting evidence; it goes after the
	// threshold so that doesn't read as the evidence's threshold.
	detail, note, _ := strings.Cut(detail, "\n")
	fmt.Fprintf(&b, "%s %s · %s\n%s", title, r.Name, e.nodeName(k.node), detail)
	if r.Threshold != nil && !strings.HasPrefix(title, "🟢") {
		fmt.Fprintf(&b, "（阈值 %s %s", r.Op, fmtValue(r.Metric, *r.Threshold))
		if r.Ratio > 0 {
			other := "出站"
			if !r.Inbound() {
				other = "入站"
			}
			fmt.Fprintf(&b, "，且不低于%s的 %g 倍", other, r.Ratio)
		}
		b.WriteString("）")
	}
	if extra != "" {
		b.WriteString("，" + extra)
	}
	if note != "" {
		b.WriteString("\n" + note)
	}
	b.WriteString("\n" + now.In(e.cfg.Location).Format("2006-01-02 15:04:05"))
	return b.String()
}

// observe evaluates a non-traffic rule on every node it applies to. Nodes
// or targets without usable data produce no observation.
func (e *Evaluator) observe(r *config.Rule, snap *snapshot, now time.Time) []obs {
	var out []obs
	for _, n := range e.cfg.Nodes {
		if !r.Covers(n.ID) {
			continue
		}
		st := snap.status[n.ID]
		if r.Metric == config.MetricOffline {
			if now.Sub(e.started) < StartupGrace {
				continue
			}
			last := e.started // never reported: count from server start
			if st != nil {
				last = time.Unix(st.FreshAt, 0)
			}
			silent := now.Sub(last)
			detail := "已 " + fmtDur(silent) + " 没有上报"
			if st == nil {
				detail = "从未上报"
			} else if silent < time.Duration(r.For) {
				detail = "已恢复上报"
			} else if p, ok := snap.peak[n.ID]; ok && e.floodBefore(n.ID, p) {
				detail += fmt.Sprintf("；停止上报前入站 %s、出站 %s，疑似被攻击后遭商家黑洞",
					fmtMbps(p.in), fmtMbps(p.out))
			}
			out = append(out, obs{node: n.ID, value: silent.Seconds(), cond: silent >= time.Duration(r.For), detail: detail})
			continue
		}
		if st == nil || now.Sub(time.Unix(st.FreshAt, 0)) > time.Duration(e.cfg.OfflineAfter) {
			continue // offline: its last values are stale
		}
		add := func(target string, v float64, label string) {
			out = append(out, obs{node: n.ID, target: target, value: v,
				cond: compare(v, r.Op, *r.Threshold), detail: label + " " + fmtValue(r.Metric, v)})
		}
		switch r.Metric {
		case config.MetricCPU:
			if st.CPU != nil {
				add("", *st.CPU, "CPU")
			}
		case config.MetricSteal:
			if st.Steal != nil {
				add("", *st.Steal, "Steal")
			}
		case config.MetricSoftIRQ:
			if st.SoftIRQ != nil {
				add("", *st.SoftIRQ, "软中断")
			}
		case config.MetricLoad1:
			if st.Load1 != nil {
				add("", *st.Load1, "负载(1m)")
			}
		case config.MetricMem:
			if st.MemTotal != nil && *st.MemTotal > 0 && st.MemUsed != nil {
				add("", 100*float64(*st.MemUsed)/float64(*st.MemTotal), "内存")
			}
		case config.MetricSwap:
			if st.SwapTotal != nil && *st.SwapTotal > 0 && st.SwapUsed != nil {
				add("", 100*float64(*st.SwapUsed)/float64(*st.SwapTotal), "Swap")
			}
		case config.MetricDisk:
			for _, d := range st.Disks {
				if d.Total > 0 {
					add(d.Mount, 100*float64(d.Used)/float64(d.Total), "磁盘 "+d.Mount)
				}
			}
		case config.MetricPPSIn, config.MetricPPSOut:
			rt, ok := snap.net[n.ID]
			if !ok || !rt.pkts {
				continue // agent before 0.1.14
			}
			own, other := rt.pin, rt.pout
			if !r.Inbound() {
				own, other = other, own
			}
			detail := rt.packets()
			if st.SoftIRQ != nil {
				detail += fmt.Sprintf("，软中断 %.1f%%", *st.SoftIRQ)
			}
			out = append(out, obs{node: n.ID, value: own, cond: compare(own, r.Op, *r.Threshold) && own >= r.Ratio*other,
				detail: detail})
		case config.MetricNetIn, config.MetricNetOut:
			rt, ok := snap.net[n.ID]
			if !ok {
				continue
			}
			own, other := rt.in, rt.out
			if r.Metric == config.MetricNetOut {
				own, other = other, own
			}
			cond := compare(own, r.Op, *r.Threshold) && own >= r.Ratio*other
			detail := fmt.Sprintf("入站 %s，出站 %s", fmtMbps(rt.in), fmtMbps(rt.out))
			if !cond && r.Ratio > 0 && r.Metric == config.MetricNetIn {
				// An inbound-only null-route ends the flood at the NIC while
				// the agent's reports still get out: the alert would recover
				// although nothing can reach the node.
				if lossy, measured := fanIn(n.ID, snap); measured > 0 && lossy*2 >= measured {
					detail += fmt.Sprintf("\n入站已回落，但 %d 个节点中 %d 个到它仍丢包 ≥ %.0f%%，可能已被商家黑洞",
						measured, lossy, fanInLoss)
				}
			}
			if cond && r.Ratio > 0 {
				what := "疑似 DDoS"
				if r.Metric == config.MetricNetOut {
					what = "疑似被利用对外攻击"
				}
				detail = what + "：" + detail
				if other > 0 {
					detail += fmt.Sprintf("（%.1f 倍）", own/other)
				}
				if r.Metric == config.MetricNetIn {
					if lossy, measured := fanIn(n.ID, snap); measured > 0 {
						detail += fmt.Sprintf("\n%d 个节点中 %d 个到它丢包 ≥ %.0f%%", measured, lossy, fanInLoss)
					}
					if ev := packetEvidence(rt, st); ev != "" {
						detail += "\n" + ev
					}
				}
			}
			out = append(out, obs{node: n.ID, value: own, cond: cond, detail: detail})
		case config.MetricPingLoss, config.MetricPingAvg:
			for _, l := range snap.links {
				if l.Src != n.ID || l.Sent == 0 || e.reportedDown(l.Dst, snap, now) {
					continue
				}
				label := fmt.Sprintf("%s → %s", n.ID, l.Dst)
				if r.Metric == config.MetricPingLoss {
					add(l.Dst, l.LossPct, label+" 丢包")
				} else if l.Avg != nil {
					add(l.Dst, *l.Avg, label+" 时延")
				}
			}
		}
	}
	return out
}

// reportedDown is true when dst is offline and an offline rule covers it.
// Every link to a dead node loses all its pings; the offline alert already
// says so, and one alert per peer would only repeat it. A target that still
// reports but can't be pinged is not down here, so those links alert.
func (e *Evaluator) reportedDown(dst string, snap *snapshot, now time.Time) bool {
	if _, ok := e.cfg.Node(dst); !ok {
		return false
	}
	if st := snap.status[dst]; st != nil && now.Sub(time.Unix(st.FreshAt, 0)) <= time.Duration(e.cfg.OfflineAfter) {
		return false
	}
	for i := range e.cfg.Alerts {
		if r := &e.cfg.Alerts[i]; r.Metric == config.MetricOffline && r.Covers(dst) {
			return true
		}
	}
	return false
}

// fanIn counts the nodes pinging id over the last minute and those of
// them losing at least fanInLoss: a flood fills the node's inbound link,
// so every peer sees loss at once, while a bad route affects only some.
func fanIn(id string, snap *snapshot) (lossy, measured int) {
	for _, l := range snap.links {
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

// floodBefore reports whether the busiest inbound minute before a node
// went silent meets a net_in rule with a ratio that covers the node.
func (e *Evaluator) floodBefore(id string, p rate) bool {
	for _, r := range e.cfg.Alerts {
		if r.Metric == config.MetricNetIn && r.Ratio > 0 && r.Covers(id) &&
			compare(p.in, r.Op, *r.Threshold) && p.in >= r.Ratio*p.out {
			return true
		}
	}
	return false
}

// traffic notifies once per period when usage crosses a quota level.
func (e *Evaluator) traffic(r *config.Rule, snap *snapshot, now time.Time,
	emit func(key, string, float64, string), persist func(key, *instance)) {
	for _, n := range e.cfg.Nodes {
		st := snap.status[n.ID]
		if !r.Covers(n.ID) || n.QuotaGB <= 0 || st == nil || st.Traffic == nil {
			continue
		}
		t := st.Traffic
		quota := n.QuotaGB * (1 << 30)
		used := float64(billable(t.RX, t.TX, n.QuotaMode))
		pct := 100 * used / quota
		k := key{r.Name, n.ID, t.Start}
		in := e.inst[k]
		prev := 0.0
		if in != nil {
			prev = in.value
		}
		level := 0.0
		for _, l := range r.Levels {
			if pct >= l && l > prev {
				level = l
			}
		}
		if level == 0 {
			continue
		}
		if in == nil {
			in = &instance{state: stateLevel, since: now}
			e.inst[k] = in
		}
		in.value, in.notified = level, now
		msg := fmt.Sprintf("📊 流量 %s · %s\n本周期（%s 起）已用 %s，达到配额 %s 的 %.0f%%（计费：%s）\n%s",
			r.Name, e.nodeName(n.ID), t.Start, fmtBytes(used), fmtBytes(quota), pct, quotaModes[n.QuotaMode],
			now.In(e.cfg.Location).Format("2006-01-02 15:04:05"))
		emit(k, "level", level, msg)
		persist(k, in)
	}
}

// expiry reminds once per level (days before the date) per expiry date.
// A renewed or rolled-forward date starts over.
func (e *Evaluator) expiry(r *config.Rule, now time.Time,
	emit func(key, string, float64, string), persist func(key, *instance), drop func(key)) {
	for i := range e.cfg.Nodes {
		n := &e.cfg.Nodes[i]
		date, days, ok := n.Expiry(now, e.cfg.Location)
		if !r.Covers(n.ID) || !ok {
			continue
		}
		k := key{r.Name, n.ID, date}
		drop(k)
		in := e.inst[k]
		prev := math.Inf(1)
		if in != nil {
			prev = in.value
		}
		level := math.Inf(1) // the most urgent level reached and not yet sent
		for _, l := range r.Levels {
			if float64(days) <= l && l < prev {
				level = min(level, l)
			}
		}
		if math.IsInf(level, 1) {
			continue
		}
		if in == nil {
			in = &instance{state: stateLevel, since: now}
			e.inst[k] = in
		}
		in.value, in.notified = level, now
		var when string
		switch {
		case days > 0:
			when = fmt.Sprintf("将于 %s 到期，还剩 %d 天", date, days)
		case days == 0:
			when = fmt.Sprintf("今天（%s）到期", date)
		default:
			when = fmt.Sprintf("已于 %s 到期（%d 天前）", date, -days)
		}
		if n.Price != "" {
			when += "，价格 " + n.Price
		}
		if n.RenewMonths > 0 {
			when += fmt.Sprintf("（自动续费，每 %d 个月）", n.RenewMonths)
		}
		emit(k, "level", float64(days), fmt.Sprintf("⏰ 到期 %s · %s\n%s\n%s",
			r.Name, e.nodeName(n.ID), when, now.In(e.cfg.Location).Format("2006-01-02 15:04:05")))
		persist(k, in)
	}
}

// ipChange notices when a node's reports start coming from another IP.
// The first IP seen for a node is recorded silently.
func (e *Evaluator) ipChange(r *config.Rule, snap *snapshot, now time.Time,
	emit func(key, string, float64, string), persist func(key, *instance), drop func(key)) {
	for _, n := range e.cfg.Nodes {
		st := snap.status[n.ID]
		if !r.Covers(n.ID) || st == nil || st.IP == "" {
			continue
		}
		k := key{r.Name, n.ID, st.IP}
		if e.inst[k] != nil {
			continue
		}
		var old string
		for o := range e.inst {
			if o.rule == r.Name && o.node == n.ID {
				old = o.target
			}
		}
		drop(k)
		in := &instance{state: stateSeen, since: time.Unix(st.IPSince, 0), notified: now}
		e.inst[k] = in
		persist(k, in)
		if old != "" {
			emit(k, "changed", 0, fmt.Sprintf("🔁 IP 变化 %s · %s\n%s → %s\n%s",
				r.Name, e.nodeName(n.ID), old, st.IP, now.In(e.cfg.Location).Format("2006-01-02 15:04:05")))
		}
	}
}

type (
	emitFn    = func(key, string, float64, string)
	persistFn = func(key, *instance)
)

// periodReport sends a node's totals for a period once it has ended,
// noticed by the newest period start changing. The first start seen for a
// node is recorded silently.
func (e *Evaluator) periodReport(ctx context.Context, r *config.Rule, snap *snapshot, now time.Time,
	emit emitFn, persist persistFn, drop func(key)) error {
	for i := range e.cfg.Nodes {
		n := &e.cfg.Nodes[i]
		st := snap.status[n.ID]
		if !r.Covers(n.ID) || st == nil || st.Traffic == nil {
			continue
		}
		cur := st.Traffic.Start
		k := key{r.Name, n.ID, cur}
		if e.inst[k] != nil {
			continue
		}
		var old string
		for o := range e.inst {
			if o.rule == r.Name && o.node == n.ID {
				old = o.target
			}
		}
		var msg string
		var ended store.Period
		var pct float64
		if old != "" && old < cur { // a smaller start: agent state rebuilt or reset day changed
			ps, err := e.src.Periods(ctx, n.ID, 2)
			if err != nil {
				return err
			}
			if len(ps) == 2 && ps[0].Start == cur {
				days, err := e.src.Daily(ctx, n.ID, ps[1].Start)
				if err != nil {
					return err
				}
				ended = ps[1]
				msg, pct = e.periodMessage(r, n, ended, cur, days, now)
			}
		}
		drop(k)
		in := &instance{state: stateSent, since: now, notified: now}
		e.inst[k] = in
		persist(k, in)
		if msg != "" {
			emit(key{r.Name, n.ID, ended.Start}, "report", pct, msg)
		}
	}
	return nil
}

func (e *Evaluator) periodMessage(r *config.Rule, n *config.Node, p store.Period, end string,
	days []store.DayTraffic, now time.Time) (string, float64) {
	var b strings.Builder
	total := float64(p.RX + p.TX)
	fmt.Fprintf(&b, "📊 流量结算 %s · %s\n%s 至 %s", r.Name, e.nodeName(n.ID), p.Start, end)
	length := 0
	if s, err := time.Parse(time.DateOnly, p.Start); err == nil {
		if t, err := time.Parse(time.DateOnly, end); err == nil {
			length = int(t.Sub(s).Hours()/24 + 0.5)
		}
	}
	if length > 0 {
		fmt.Fprintf(&b, "（%d 天）", length)
	}
	fmt.Fprintf(&b, "：下行 %s，上行 %s，合计 %s", fmtBytes(float64(p.RX)), fmtBytes(float64(p.TX)), fmtBytes(total))
	pct := 0.0
	if n.QuotaGB > 0 {
		quota := n.QuotaGB * (1 << 30)
		pct = 100 * float64(billable(p.RX, p.TX, n.QuotaMode)) / quota
		fmt.Fprintf(&b, "\n配额 %s（计费：%s），用了 %.1f%%", fmtBytes(quota), quotaModes[n.QuotaMode], pct)
	}
	if length > 0 {
		// Not divided by the number of daily rows: with a reset_time other
		// than 00:00 the period's last half day has none.
		fmt.Fprintf(&b, "\n日均 %s", fmtBytes(total/float64(length)))
		var peak *store.DayTraffic
		for i := range days {
			if peak == nil || days[i].RX+days[i].TX > peak.RX+peak.TX {
				peak = &days[i]
			}
		}
		if peak != nil {
			fmt.Fprintf(&b, "，最多的一天 %s（%s）", peak.Day, fmtBytes(float64(peak.RX+peak.TX)))
		}
	}
	b.WriteString("\n" + now.In(e.cfg.Location).Format("2006-01-02 15:04:05"))
	return b.String(), pct
}

// weeklyReport sends one summary of all covered nodes at the rule's weekly
// slot, or up to a day late if the server was down then.
func (e *Evaluator) weeklyReport(ctx context.Context, r *config.Rule, snap *snapshot, now time.Time,
	emit emitFn, persist persistFn, drop func(key)) error {
	slot := r.WeeklySlot(now, e.cfg.Location)
	if now.Sub(slot) >= 24*time.Hour {
		return nil
	}
	k := key{r.Name, "", slot.Format("2006-01-02 15:04")}
	if e.inst[k] != nil {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📊 每周流量 %s", r.Name)
	var soon []string
	for i := range e.cfg.Nodes {
		n := &e.cfg.Nodes[i]
		if !r.Covers(n.ID) {
			continue
		}
		line, err := e.weeklyLine(ctx, n, snap.status[n.ID], now)
		if err != nil {
			return err
		}
		b.WriteString("\n" + line)
		if date, days, ok := n.Expiry(now, e.cfg.Location); ok && days <= 30 {
			if days >= 0 {
				soon = append(soon, fmt.Sprintf("%s %s（还剩 %d 天）", e.nodeName(n.ID), date, days))
			} else {
				soon = append(soon, fmt.Sprintf("%s %s（已过期 %d 天）", e.nodeName(n.ID), date, -days))
			}
		}
	}
	if len(soon) > 0 {
		b.WriteString("\n即将到期：" + strings.Join(soon, "、"))
	}
	b.WriteString("\n" + now.In(e.cfg.Location).Format("2006-01-02 15:04:05"))
	drop(k)
	in := &instance{state: stateSent, since: now, notified: now}
	e.inst[k] = in
	persist(k, in)
	emit(k, "report", 0, b.String())
	return nil
}

// weeklyLine is one node's line: usage this period, the last 7 whole days,
// and the period's end usage projected at the last 7 days' daily rate.
func (e *Evaluator) weeklyLine(ctx context.Context, n *config.Node, st *store.Status, now time.Time) (string, error) {
	name := e.nodeName(n.ID)
	if st == nil || st.Traffic == nil {
		return name + "：暂无流量数据", nil
	}
	ps, err := e.src.Periods(ctx, n.ID, 2)
	if err != nil {
		return "", err
	}
	if len(ps) == 0 {
		return name + "：暂无流量数据", nil
	}
	cur := ps[0]
	loc := e.cfg.Location
	y, m, d := now.In(loc).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, loc)
	from, to := today.AddDate(0, 0, -7).Format(time.DateOnly), today.Format(time.DateOnly)
	var rx7, tx7 int64
	whole := map[string]bool{}
	for _, p := range ps { // the last 7 days may span the previous period
		days, err := e.src.Daily(ctx, n.ID, p.Start)
		if err != nil {
			return "", err
		}
		for _, d := range days {
			if d.Day >= from && d.Day < to {
				rx7, tx7 = rx7+d.RX, tx7+d.TX
				whole[d.Day] = true
			}
		}
	}

	quota := n.QuotaGB * (1 << 30)
	used := func(rx, tx float64) string {
		if quota <= 0 {
			return fmtBytes(rx + tx)
		}
		v := float64(billable(int64(rx), int64(tx), n.QuotaMode))
		return fmt.Sprintf("%s / %s（%.1f%%）", fmtBytes(v), fmtBytes(quota), 100*v/quota)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s：本周期（%s 起）%s", name, cur.Start, used(float64(cur.RX), float64(cur.TX)))
	if len(whole) > 0 { // fewer than 7 for a node that started reporting lately
		fmt.Fprintf(&b, "，近 %d 天 %s", len(whole), fmtBytes(float64(rx7+tx7)))
	}
	next, ok := e.nextReset(n, cur.Start)
	if !ok {
		return b.String(), nil // the server's reset day doesn't match the agent's periods
	}
	remain := next.Sub(now).Hours() / 24
	if remain <= 0 {
		return b.String(), nil // offline across its reset: no newer period reported yet
	}
	if len(whole) > 0 {
		// rx and tx projected separately: billable() of the sums is what a
		// max quota bills.
		k := float64(len(whole))
		prx, ptx := float64(cur.RX)+float64(rx7)/k*remain, float64(cur.TX)+float64(tx7)/k*remain
		fmt.Fprintf(&b, "，预计周期末 %s", used(prx, ptx))
		if quota > 0 && float64(billable(int64(prx), int64(ptx), n.QuotaMode)) >= quota {
			b.WriteString(" ⚠️ 可能超额")
		}
	}
	layout := "01-02"
	if next.Hour() != 0 || next.Minute() != 0 {
		layout = "01-02 15:04"
	}
	fmt.Fprintf(&b, "，%s 重置", next.Format(layout))
	return b.String(), nil
}

// nextReset is when the period starting on date start ends, from the
// node's reset day and time in the server config. Those only feed
// agent-config; if they don't reproduce start, the agent's own differ.
func (e *Evaluator) nextReset(n *config.Node, start string) (time.Time, bool) {
	r := traffic.Reset{Day: n.ResetDay}
	if t, err := time.Parse("15:04", n.ResetTime); err == nil {
		r.Hour, r.Minute = t.Hour(), t.Minute()
	}
	loc := e.cfg.Location
	at, err := time.ParseInLocation("2006-01-02 15:04", fmt.Sprintf("%s %02d:%02d", start, r.Hour, r.Minute), loc)
	if err != nil || !traffic.PeriodStart(at, loc, r).Equal(at) {
		return time.Time{}, false
	}
	return traffic.NextPeriodStart(at, loc, r), true
}

var quotaModes = map[string]string{"sum": "收+发", "max": "取大", "tx": "仅上行", "rx": "仅下行"}

func billable(rx, tx int64, mode string) int64 {
	switch mode {
	case "max":
		return max(rx, tx)
	case "tx":
		return tx
	case "rx":
		return rx
	}
	return rx + tx
}

func compare(v float64, op string, th float64) bool {
	switch op {
	case ">":
		return v > th
	case ">=":
		return v >= th
	case "<":
		return v < th
	case "<=":
		return v <= th
	}
	return false
}

func fmtValue(metric string, v float64) string {
	switch metric {
	case config.MetricLoad1:
		return fmt.Sprintf("%.2f", v)
	case config.MetricPingAvg:
		return fmt.Sprintf("%.1f ms", v)
	case config.MetricNetIn, config.MetricNetOut:
		return fmtMbps(v)
	case config.MetricPPSIn, config.MetricPPSOut:
		return fmtPPS(v)
	}
	return fmt.Sprintf("%.1f%%", v)
}

func fmtMbps(v float64) string { return fmt.Sprintf("%.1f Mbps", v) }

func fmtPPS(v float64) string {
	if v >= 1e4 {
		return fmt.Sprintf("%.1f 万包/秒", v/1e4)
	}
	return fmt.Sprintf("%.0f 包/秒", v)
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%d 天 %d 小时", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d 秒", int(d.Seconds()))
}

// fmtBytes stops at GB: quotas are configured in GB, and "1.12 TB / 1000 GB"
// is harder to compare than "1146.88 GB / 1000.00 GB".
func fmtBytes(b float64) string {
	units := []string{"B", "KB", "MB", "GB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", b)
	}
	return fmt.Sprintf("%.2f %s", math.Round(b*100)/100, units[i])
}

// Rules lists rule names, for the API.
func (e *Evaluator) Rules() []config.Rule { return slices.Clone(e.cfg.Alerts) }

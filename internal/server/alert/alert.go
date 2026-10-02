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
)

// Source is what the evaluator needs from the store.
type Source interface {
	Status(ctx context.Context, node string) (*store.Status, error)
	Matrix(ctx context.Context, window time.Duration) ([]store.Link, error)
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
		if _, ok := e.cfg.Node(s.Node); !ok {
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
		if in.state == stateLevel || in.state == stateSeen {
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

type snapshot struct {
	status map[string]*store.Status
	links  []store.Link
}

func (e *Evaluator) snapshot(ctx context.Context) (*snapshot, error) {
	s := &snapshot{status: map[string]*store.Status{}}
	for _, n := range e.cfg.Nodes {
		st, err := e.src.Status(ctx, n.ID)
		if err != nil {
			return nil, err
		}
		s.status[n.ID] = st
	}
	var err error
	s.links, err = e.src.Matrix(ctx, pingWindow)
	return s, err
}

// Tick runs one evaluation round and sends one merged message for it.
func (e *Evaluator) Tick(ctx context.Context) error {
	snap, err := e.snapshot(ctx)
	if err != nil {
		return err
	}
	now := e.now()

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
	fmt.Fprintf(&b, "%s %s · %s\n%s", title, r.Name, e.nodeName(k.node), detail)
	if r.Threshold != nil && !strings.HasPrefix(title, "🟢") {
		fmt.Fprintf(&b, "（阈值 %s %s）", r.Op, fmtValue(r.Metric, *r.Threshold))
	}
	if extra != "" {
		b.WriteString("，" + extra)
	}
	b.WriteString("\n" + now.In(e.cfg.Location).Format("2006-01-02 15:04:05"))
	return b.String()
}

// observe evaluates a non-traffic rule on every node it applies to. Nodes
// or targets without usable data produce no observation.
func (e *Evaluator) observe(r *config.Rule, snap *snapshot, now time.Time) []obs {
	var out []obs
	for _, n := range e.cfg.Nodes {
		if !r.Nodes.Has(n.ID) {
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
		if r := &e.cfg.Alerts[i]; r.Metric == config.MetricOffline && r.Nodes.Has(dst) {
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
		if !r.Nodes.Has(n.ID) || n.QuotaGB <= 0 || st == nil || st.Traffic == nil {
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
		if !r.Nodes.Has(n.ID) || !ok {
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
		if !r.Nodes.Has(n.ID) || st == nil || st.IP == "" {
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
	}
	return fmt.Sprintf("%.1f%%", v)
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

func fmtBytes(b float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
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

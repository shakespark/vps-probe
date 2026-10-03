package alert

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/shakespark/vps-probe/internal/server/config"
	"github.com/shakespark/vps-probe/internal/server/store"
)

type fakeSrc struct {
	status map[string]*store.Status
	links  []store.Link
	net    map[string][]store.NetSum
	period map[string][]store.Period     // newest first
	daily  map[string][]store.DayTraffic // node + "|" + period start
	states map[key]store.AlertState
	marks  map[markKey]store.ReportMark
	events []store.AlertEvent
}

func (f *fakeSrc) Status(_ context.Context, id string) (*store.Status, error) {
	return f.status[id], nil
}
func (f *fakeSrc) Matrix(context.Context, time.Duration) ([]store.Link, error) { return f.links, nil }
func (f *fakeSrc) NetSums(_ context.Context, node string, from, to int64) ([]store.NetSum, error) {
	var out []store.NetSum
	for _, n := range f.net[node] {
		if n.TS >= from && n.TS <= to {
			out = append(out, n)
		}
	}
	return out, nil
}
func (f *fakeSrc) Periods(_ context.Context, node string, limit int) ([]store.Period, error) {
	p := f.period[node]
	return p[:min(limit, len(p))], nil
}
func (f *fakeSrc) Daily(_ context.Context, node string, start int64) ([]store.DayTraffic, error) {
	return f.daily[fmt.Sprint(node, "|", start)], nil
}
func (f *fakeSrc) AlertStates() (states []store.AlertState, marks []store.ReportMark, err error) {
	for _, s := range f.states {
		states = append(states, s)
	}
	for _, m := range f.marks {
		marks = append(marks, m)
	}
	return states, marks, nil
}
func (f *fakeSrc) SaveAlerts(c store.AlertChanges) error {
	for _, s := range c.Put {
		f.states[key{s.Rule, s.Node, s.Target}] = s
	}
	for _, s := range c.Del {
		delete(f.states, key{s.Rule, s.Node, s.Target})
	}
	for _, m := range c.Marks {
		f.marks[markKey{m.Report, m.Node}] = m
	}
	f.events = append(f.events, c.Events...)
	return nil
}

type fakeNotifier struct{ msgs []string }

func (n *fakeNotifier) Notify(text string) { n.msgs = append(n.msgs, text) }

const tokA = "abcdefghijklmnopqrstuvwxyz0123456789"
const tokB = "bcdefghijklmnopqrstuvwxyz0123456789a"

type harness struct {
	t   *testing.T
	cfg *config.Config
	src *fakeSrc
	n   *fakeNotifier
	e   *Evaluator
	now time.Time
}

const twoNodes = `
  - {id: a, name: 香港, token: ` + tokA + `, traffic: {quota_gb: 100}}
  - {id: b, token: ` + tokB + `}`

// setup has nodes a and b, the given alert rules and no reports.
func setup(t *testing.T, rules string) *harness {
	t.Helper()
	return setupConfig(t, "nodes:"+twoNodes+"\nreports: []\nalerts:\n"+rules)
}

// setupReports has the given nodes and reports and no alert rules.
func setupReports(t *testing.T, nodes, reports string) *harness {
	t.Helper()
	return setupConfig(t, "nodes:"+nodes+"\nalerts: []\nreports:\n"+reports)
}

func setupConfig(t *testing.T, yml string) *harness {
	t.Helper()
	cfg, err := config.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, cfg: cfg, src: &fakeSrc{status: map[string]*store.Status{}, states: map[key]store.AlertState{},
		marks: map[markKey]store.ReportMark{}},
		n: &fakeNotifier{}, now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	h.newEvaluator()
	return h
}

func (h *harness) newEvaluator() {
	h.e = New(h.cfg, h.src, h.n, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.e.now = func() time.Time { return h.now }
	if err := h.e.Load(); err != nil {
		h.t.Fatal(err)
	}
}

// report makes node fresh at the current time with the given CPU.
func (h *harness) report(node string, cpu float64) {
	st := h.src.status[node]
	if st == nil {
		st = &store.Status{}
		h.src.status[node] = st
	}
	st.FreshAt = h.now.Unix()
	st.CPU = &cpu
}

// traffic makes node fresh at the current time with one network sample,
// in Mbps.
func (h *harness) traffic(node string, in, out float64) {
	h.report(node, 1)
	h.src.status[node].MaxTS = h.now.Unix()
	if h.src.net == nil {
		h.src.net = map[string][]store.NetSum{}
	}
	h.src.net[node] = append(h.src.net[node], store.NetSum{TS: h.now.Unix(), RX: in * 1e6 / 8, TX: out * 1e6 / 8})
}

// packets adds packet rates to node's newest network sample.
func (h *harness) packets(node string, in, out float64) {
	n := &h.src.net[node][len(h.src.net[node])-1]
	n.RXPkts, n.TXPkts = in, out
}

// step advances the clock, optionally reporting, and runs one round.
func (h *harness) step(d time.Duration, report func()) {
	h.now = h.now.Add(d)
	if report != nil {
		report()
	}
	if err := h.e.Tick(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) msgs() int { return len(h.n.msgs) }

func TestCPUStateMachine(t *testing.T) {
	h := setup(t, `  - {name: cpu_high, metric: cpu, op: ">", threshold: 90, for: 1m, repeat: 10m}`)
	hot := func() { h.report("a", 95) }
	cool := func() { h.report("a", 10) }

	h.step(0, hot)
	for i := 0; i < 5; i++ {
		h.step(10*time.Second, hot)
	}
	if h.msgs() != 0 || h.e.Active()[0].Firing {
		t.Fatalf("fired before for elapsed: %v", h.n.msgs)
	}
	h.step(10*time.Second, hot) // 60s
	if h.msgs() != 1 || !strings.Contains(h.n.msgs[0], "🔴 告警 cpu_high · 香港（a）") ||
		!strings.Contains(h.n.msgs[0], "CPU 95.0%（阈值 > 90.0%）") {
		t.Fatalf("firing message: %q", h.n.msgs)
	}

	// Brief dips shorter than the debounce don't recover.
	h.step(10*time.Second, cool)
	h.step(10*time.Second, cool)
	h.step(10*time.Second, hot)
	if h.msgs() != 1 {
		t.Fatalf("flapped: %v", h.n.msgs)
	}
	// Reminder after repeat.
	for i := 0; i < 60; i++ {
		h.step(10*time.Second, hot)
	}
	if h.msgs() != 2 || !strings.HasPrefix(h.n.msgs[1], "🟠 仍在告警") {
		t.Fatalf("repeat: %v", h.n.msgs)
	}
	// Sustained recovery: condition false for min(for, 1m) = 60s.
	for i := 0; i < 6; i++ {
		h.step(10*time.Second, cool)
	}
	if h.msgs() != 2 {
		t.Fatalf("recovered before the debounce: %v", h.n.msgs)
	}
	for i := 0; i < 1; i++ {
		h.step(10*time.Second, cool)
	}
	if h.msgs() != 3 || !strings.HasPrefix(h.n.msgs[2], "🟢 恢复 cpu_high") {
		t.Fatalf("recovery: %v", h.n.msgs)
	}
	if len(h.src.states) != 0 || len(h.e.Active()) != 0 {
		t.Fatalf("state left: %v", h.src.states)
	}
	ev := h.src.events
	if len(ev) != 3 || ev[0].Event != "firing" || ev[1].Event != "repeat" || ev[2].Event != "recovered" {
		t.Fatalf("history: %+v", ev)
	}
}

func TestPendingResetsWhenConditionClears(t *testing.T) {
	h := setup(t, `  - {name: cpu_high, metric: cpu, op: ">", threshold: 90, for: 1m}`)
	h.step(0, func() { h.report("a", 95) })
	h.step(30*time.Second, func() { h.report("a", 50) })
	h.step(30*time.Second, func() { h.report("a", 95) })
	h.step(30*time.Second, func() { h.report("a", 95) })
	if h.msgs() != 0 {
		t.Fatalf("pending did not restart: %v", h.n.msgs)
	}
}

func TestOfflineNodeHoldsFiringAndDropsPending(t *testing.T) {
	h := setup(t, `  - {name: cpu_high, metric: cpu, op: ">", threshold: 90, for: 40s}`)
	h.step(0, func() { h.report("a", 95); h.report("b", 95) })
	h.step(20*time.Second, func() { h.report("a", 95) }) // b goes silent while pending
	h.step(20*time.Second, func() { h.report("a", 95) }) // a fires; b is stale, its pending dropped
	if h.msgs() != 1 {
		t.Fatalf("msgs: %v", h.n.msgs)
	}
	// a stops reporting too: stale values must neither recover nor re-fire.
	for i := 0; i < 20; i++ {
		h.step(10*time.Second, nil)
	}
	act := h.e.Active()
	if h.msgs() != 1 || len(act) != 1 || act[0].Node != "a" || !act[0].Firing {
		t.Fatalf("after silence: msgs=%v active=%+v", h.n.msgs, act)
	}
}

func TestOffline(t *testing.T) {
	h := setup(t, `  - {name: offline, metric: offline, for: 1m}`)
	fresh := func() { h.report("a", 1) }
	h.step(0, fresh)
	// b never reports; within the startup grace nothing fires.
	for i := 0; i < 11; i++ {
		h.step(10*time.Second, fresh)
	}
	if h.msgs() != 0 {
		t.Fatalf("fired during grace: %v", h.n.msgs)
	}
	h.step(10*time.Second, fresh) // 2m: b silent since start
	if h.msgs() != 1 || !strings.Contains(h.n.msgs[0], "· b\n从未上报") {
		t.Fatalf("never-reported: %q", h.n.msgs)
	}
	// a goes silent for 1m.
	for i := 0; i < 6; i++ {
		h.step(10*time.Second, nil)
	}
	if h.msgs() != 2 || !strings.Contains(h.n.msgs[1], "已 1 分 0 秒 没有上报") {
		t.Fatalf("offline: %q", h.n.msgs)
	}
	// One fresh report recovers at once.
	h.step(10*time.Second, fresh)
	if h.msgs() != 3 || !strings.HasPrefix(h.n.msgs[2], "🟢 恢复 offline · 香港（a）\n已恢复上报，离线约 1 分 10 秒\n") {
		t.Fatalf("recovery: %q", h.n.msgs)
	}
}

func TestOneMessagePerRound(t *testing.T) {
	h := setup(t, `  - {name: cpu, metric: cpu, op: ">", threshold: 90}
  - {name: disk, metric: disk, op: ">", threshold: 80}`)
	h.step(0, func() {
		h.report("a", 99)
		h.src.status["a"].Disks = []store.Disk{{Mount: "/", Total: 100, Used: 85}, {Mount: "/data", Total: 100, Used: 10}}
		h.report("b", 99)
	})
	if h.msgs() != 1 || strings.Count(h.n.msgs[0], "🔴") != 3 || !strings.Contains(h.n.msgs[0], "磁盘 / 85.0%") {
		t.Fatalf("batch: %q", h.n.msgs)
	}
}

func TestPingLoss(t *testing.T) {
	h := setup(t, `  - {name: loss, metric: ping_loss, op: ">", threshold: 20}
  - {name: rtt, metric: ping_avg, op: ">", threshold: 200}`)
	avg := 150.0
	h.src.links = []store.Link{{Src: "a", Dst: "b", Sent: 60, Lost: 30, LossPct: 50, Avg: &avg},
		{Src: "a", Dst: "void", Sent: 60, Lost: 60, LossPct: 100}}
	h.step(0, func() { h.report("a", 1) })
	m := h.n.msgs
	if len(m) != 1 || strings.Count(m[0], "🔴 告警 loss") != 2 || strings.Contains(m[0], "rtt") ||
		!strings.Contains(m[0], "a → void 丢包 100.0%") {
		t.Fatalf("ping: %q", m)
	}
}

func TestLinksToOfflineNodeDoNotAlert(t *testing.T) {
	h := setup(t, `  - {name: offline, metric: offline, for: 1m}
  - {name: loss, metric: ping_loss, op: ">", threshold: 20, for: 3m}`)
	loss := func(pct float64) { h.src.links = []store.Link{{Src: "a", Dst: "b", Sent: 60, LossPct: pct}} }
	both := func() { h.report("a", 1); h.report("b", 1) }
	run := func(d time.Duration, report func()) {
		for i := time.Duration(0); i < d; i += 10 * time.Second {
			h.step(10*time.Second, report)
		}
	}
	loss(0)
	run(3*time.Minute, both) // past the startup grace

	// b dies: a loses every ping to it, but only the offline alert is sent.
	loss(100)
	run(10*time.Minute, func() { h.report("a", 1) })
	if h.msgs() != 1 || !strings.Contains(h.n.msgs[0], "🔴 告警 offline · b") {
		t.Fatalf("b down: %q", h.n.msgs)
	}
	// b is back; the 60s loss window still holds the outage for a while.
	loss(50)
	run(time.Minute, both)
	loss(0)
	run(5*time.Minute, both)
	if h.msgs() != 2 || !strings.HasPrefix(h.n.msgs[1], "🟢 恢复 offline · b") {
		t.Fatalf("b back: %q", h.n.msgs)
	}

	// b reports but can't be pinged: that is worth an alert.
	loss(100)
	run(3*time.Minute+10*time.Second, both) // pending starts on the first round
	if h.msgs() != 3 || !strings.Contains(h.n.msgs[2], "🔴 告警 loss · 香港（a）\na → b 丢包 100.0%") {
		t.Fatalf("b unreachable: %q", h.n.msgs)
	}
	// Then b dies too: the link alert holds, no recovery and no repeat.
	run(5*time.Minute, func() { h.report("a", 1) })
	if h.msgs() != 4 || !strings.Contains(h.n.msgs[3], "🔴 告警 offline · b") {
		t.Fatalf("b down while link firing: %q", h.n.msgs)
	}
	loss(0)
	run(2*time.Minute, both)
	if h.msgs() != 6 || !strings.Contains(h.n.msgs[4], "🟢 恢复 offline · b") ||
		!strings.Contains(h.n.msgs[5], "🟢 恢复 loss · 香港（a）") {
		t.Fatalf("all clear: %q", h.n.msgs)
	}
}

func TestLinksToNodeWithoutOfflineRuleStillAlert(t *testing.T) {
	h := setup(t, `  - {name: offline, metric: offline, for: 1m, nodes: [a]}
  - {name: loss, metric: ping_loss, op: ">", threshold: 20, for: 3m}`)
	h.src.links = []store.Link{{Src: "a", Dst: "b", Sent: 60, LossPct: 100}}
	for i := 0; i < 19; i++ {
		h.step(10*time.Second, func() { h.report("a", 1) })
	}
	if h.msgs() != 1 || !strings.Contains(h.n.msgs[0], "a → b 丢包 100.0%") {
		t.Fatalf("b has no offline rule, the link must alert: %q", h.n.msgs)
	}
}

// Period boundaries: the first of September, October and November 2026 in
// the server's timezone.
var sep, oct, nov = monthStart(9), monthStart(10), monthStart(11)

func monthStart(m time.Month) int64 {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	return time.Date(2026, m, 1, 0, 0, 0, 0, sh).Unix()
}

func TestQuotaLevels(t *testing.T) {
	h := setupReports(t, twoNodes, `  - {type: traffic_quota, levels: [80, 90, 100]}`)
	gb := int64(1 << 30)
	use := func(start, end int64, g int64) func() {
		return func() {
			h.report("a", 1)
			h.report("b", 1) // no quota: skipped
			h.src.status["a"].Traffic = &store.Period{Start: start, End: end, RX: g * gb / 2, TX: g * gb / 2}
			h.src.status["b"].Traffic = &store.Period{Start: start, End: end, RX: 1 << 50}
		}
	}
	h.step(0, use(sep, oct, 50))
	h.step(10*time.Second, use(sep, oct, 85))
	h.step(10*time.Second, use(sep, oct, 86))
	if h.msgs() != 1 || !strings.Contains(h.n.msgs[0], "📊 流量 · 香港（a）\n本周期（2026-09-01 起）已用 85.00 GB，达到配额 100.00 GB 的 85%（计费：收+发）\n2026-") {
		t.Fatalf("80%%: %q", h.n.msgs)
	}
	// Jumping past two levels sends one message for the highest.
	h.step(10*time.Second, use(sep, oct, 101))
	if ev := h.src.events[1]; h.msgs() != 2 || ev.Value != "100%" || ev.Rule != "traffic_quota" || ev.Target != "2026-09-01" || ev.Event != "notice" {
		t.Fatalf("100%%: %q %+v", h.n.msgs, ev)
	}
	// Survives a restart without repeating.
	h.newEvaluator()
	h.step(10*time.Second, use(sep, oct, 102))
	if h.msgs() != 2 {
		t.Fatalf("repeated after restart: %q", h.n.msgs)
	}
	// New period re-arms.
	h.step(10*time.Second, use(oct, nov, 81))
	if h.msgs() != 3 || len(h.e.Active()) != 0 || len(h.src.marks) != 1 {
		t.Fatalf("new period: %q", h.n.msgs)
	}
}

func TestFiringSurvivesRestartAndRemovedRulesArePruned(t *testing.T) {
	h := setup(t, `  - {name: cpu_high, metric: cpu, op: ">", threshold: 90}`)
	h.step(0, func() { h.report("a", 95) })
	h.src.states[key{"old_rule", "a", ""}] = store.AlertState{Rule: "old_rule", Node: "a", Firing: true}
	h.src.states[key{"cpu_high", "gone", ""}] = store.AlertState{Rule: "cpu_high", Node: "gone", Firing: true}
	h.newEvaluator()
	if len(h.src.states) != 1 || len(h.e.Active()) != 1 {
		t.Fatalf("states of a removed rule or node kept: %+v", h.src.states)
	}
	h.step(10*time.Second, func() { h.report("a", 95) })
	if h.msgs() != 1 {
		t.Fatalf("re-fired after restart: %q", h.n.msgs)
	}
	for i := 0; i < 1; i++ {
		h.step(10*time.Second, func() { h.report("a", 5) })
	}
	if h.msgs() != 2 || !strings.HasPrefix(h.n.msgs[1], "🟢") {
		t.Fatalf("recovery after restart: %q", h.n.msgs)
	}
}

func TestMessageUsesConfiguredTimezone(t *testing.T) {
	h := setup(t, `  - {name: cpu_high, metric: cpu, op: ">", threshold: 90}`)
	h.step(0, func() { h.report("a", 95) })
	// 12:00 UTC is 20:00 in Asia/Shanghai (the config default).
	if !strings.Contains(h.n.msgs[0], "2026-09-30 20:00:00") {
		t.Fatalf("time: %q", h.n.msgs[0])
	}
}

func TestNoRecoveryMessageWhenDisabled(t *testing.T) {
	h := setup(t, `  - {name: cpu_high, metric: cpu, op: ">", threshold: 90, notify_recovery: false}`)
	h.step(0, func() { h.report("a", 95) })
	h.step(10*time.Second, func() { h.report("a", 5) })
	if h.msgs() != 1 || len(h.src.events) != 2 || h.src.events[1].Event != "recovered" {
		t.Fatalf("msgs=%q events=%+v", h.n.msgs, h.src.events)
	}
}

// Ticker jitter must not delay firing by a whole round.
func TestTickJitter(t *testing.T) {
	h := setup(t, `  - {name: cpu_high, metric: cpu, op: ">", threshold: 90, for: 20s}`)
	hot := func() { h.report("a", 95) }
	h.step(0, hot)
	h.step(10*time.Second-time.Millisecond, hot)
	h.step(10*time.Second-time.Millisecond, hot)
	if h.msgs() != 1 {
		t.Fatalf("did not fire at ~20s: %v", h.n.msgs)
	}
}

func TestExpiryLevels(t *testing.T) {
	// The clock starts at 2026-09-30 20:00 in Shanghai: a is 10 days out,
	// b expired 5 days ago, b2 renews monthly and is due today.
	h := setupReports(t, `
  - {id: a, token: `+tokA+`, plan: {expire_at: 2026-10-10, price: $5/月}}
  - {id: b, token: `+tokB+`, plan: {expire_at: 2026-09-25}}
  - {id: b2, token: `+tokA+`x, plan: {expire_at: 2026-08-30, renew_months: 1}}
  - {id: c, token: `+tokB+`x}`,
		`  - {type: expiry, days: [7, 1]}`)
	h.step(0, nil)
	// Already past every level: one message for the most urgent.
	if h.msgs() != 1 || !strings.Contains(h.n.msgs[0], "⏰ 到期 · b\n已于 2026-09-25 到期（5 天前）") ||
		!strings.Contains(h.n.msgs[0], "b2\n今天（2026-09-30）到期（自动续费，每 1 个月）") {
		t.Fatalf("first round: %q", h.n.msgs)
	}
	h.step(10*time.Second, nil)
	if h.msgs() != 1 {
		t.Fatalf("repeated: %q", h.n.msgs)
	}
	h.step(3*24*time.Hour, nil) // a: 7 days left; b2 rolled to 2026-10-30
	if h.msgs() != 2 || !strings.Contains(h.n.msgs[1], "将于 2026-10-10 到期，还剩 7 天，价格 $5/月") ||
		strings.Contains(h.n.msgs[1], "b2") {
		t.Fatalf("7 days: %q", h.n.msgs)
	}
	if ev := h.src.events[len(h.src.events)-1]; ev.Event != "notice" || ev.Value != "剩 7 天" || ev.Target != "2026-10-10" || ev.Rule != "expiry" {
		t.Fatalf("event: %+v", ev)
	}
	if len(h.e.Active()) != 0 {
		t.Fatalf("active: %+v", h.e.Active())
	}
	h.newEvaluator()
	h.step(24*time.Hour, nil) // a: 6 days
	if h.msgs() != 2 {
		t.Fatalf("after restart: %q", h.n.msgs)
	}
	h.step(5*24*time.Hour, nil) // a: 1 day
	if h.msgs() != 3 || !strings.Contains(h.n.msgs[2], "还剩 1 天") {
		t.Fatalf("1 day: %q", h.n.msgs)
	}
}

func TestIPChange(t *testing.T) {
	h := setupReports(t, twoNodes, `  - {type: ip_change}`)
	from := func(node, ip string) func() {
		return func() {
			h.report(node, 1)
			h.src.status[node].IP, h.src.status[node].IPSince = ip, h.now.Unix()
		}
	}
	h.step(0, from("a", "198.51.100.1"))
	h.step(10*time.Second, from("a", "198.51.100.1"))
	if h.msgs() != 0 || len(h.e.Active()) != 0 {
		t.Fatalf("first sighting: %q", h.n.msgs)
	}
	h.newEvaluator()
	h.step(10*time.Second, from("a", "203.0.113.9"))
	if h.msgs() != 1 || !strings.Contains(h.n.msgs[0], "🔁 IP 变化 · 香港（a）\n198.51.100.1 → 203.0.113.9\n2026-") {
		t.Fatalf("change: %q", h.n.msgs)
	}
	if ev := h.src.events[0]; ev.Event != "notice" || ev.Target != "203.0.113.9" {
		t.Fatalf("event: %+v", ev)
	}
	if len(h.src.marks) != 1 {
		t.Fatalf("marks: %+v", h.src.marks)
	}
	h.step(10*time.Second, from("a", "203.0.113.9"))
	h.newEvaluator()
	h.step(10*time.Second, from("a", "203.0.113.9"))
	if h.msgs() != 1 {
		t.Fatalf("repeated: %q", h.n.msgs)
	}
}

func TestNetInFlood(t *testing.T) {
	h := setup(t, `  - {name: ddos, metric: net_in, op: ">=", threshold: 50, ratio: 4, for: 1m}`)
	h.src.links = []store.Link{{Src: "b", Dst: "a", Sent: 60, Lost: 30, LossPct: 50},
		{Src: "a", Dst: "b", Sent: 60, Lost: 60, LossPct: 100}}
	// A relay forwarding 300 Mbps each way is busy, not attacked.
	for i := 0; i < 12; i++ {
		h.step(10*time.Second, func() { h.traffic("a", 300, 290); h.traffic("b", 1, 1) })
	}
	if h.msgs() != 0 || len(h.e.Active()) != 0 {
		t.Fatalf("symmetric load alerted: %v", h.n.msgs)
	}
	// Inbound jumps; the 60s average crosses the floor and stays lopsided.
	for i := 0; i < 12; i++ {
		h.step(10*time.Second, func() { h.traffic("a", 800, 40); h.packets("a", 100000, 5000); h.traffic("b", 1, 1) })
	}
	m := h.n.msgs
	if len(m) != 1 || !strings.Contains(m[0], "🔴 告警 ddos · 香港（a）\n疑似 DDoS：入站 800.0 Mbps，出站 40.0 Mbps（20.0 倍）（阈值 >= 50.0 Mbps，且不低于出站的 4 倍），已持续 1 分 0 秒\n"+
		"1 个节点中 1 个到它丢包 ≥ 20%\n入站 10.0 万包/秒（平均 1000 字节/包），出站 5000 包/秒\n2026-") {
		t.Fatalf("flood: %q", m)
	}
	// Back to normal: recovers after the debounce, with plain rates.
	h.src.links = nil
	for i := 0; i < 13; i++ {
		h.step(10*time.Second, func() { h.traffic("a", 20, 20); h.traffic("b", 1, 1) })
	}
	if h.msgs() != 2 || !strings.Contains(h.n.msgs[1], "🟢 恢复 ddos · 香港（a）\n入站 20.0 Mbps，出站 20.0 Mbps，异常持续约 2 分 20 秒\n2026-") {
		t.Fatalf("recovery: %q", h.n.msgs)
	}
}

func TestNetInRecoveryUnderNullRoute(t *testing.T) {
	h := setup(t, `  - {name: ddos, metric: net_in, op: ">=", threshold: 50, ratio: 4}`)
	h.step(0, func() { h.traffic("a", 800, 40) })
	// The provider drops traffic to a; its own reports still get out.
	h.src.links = []store.Link{{Src: "b", Dst: "a", Sent: 60, Lost: 60, LossPct: 100}}
	for i := 0; i < 6; i++ {
		h.step(10*time.Second, func() { h.traffic("a", 0, 0.1) })
	}
	if h.msgs() != 2 || !strings.Contains(h.n.msgs[1], "🟢 恢复 ddos · 香港（a）\n入站 0.0 Mbps，出站 0.1 Mbps，异常持续约") ||
		!strings.Contains(h.n.msgs[1], "\n入站已回落，但 1 个节点中 1 个到它仍丢包 ≥ 20%，可能已被商家黑洞\n") {
		t.Fatalf("null-route recovery: %q", h.n.msgs)
	}
}

func TestNetOutAndNoRatio(t *testing.T) {
	h := setup(t, `  - {name: abuse_out, metric: net_out, op: ">=", threshold: 50, ratio: 4}
  - {name: busy, metric: net_in, op: ">", threshold: 100}`)
	h.step(0, func() { h.traffic("a", 10, 200); h.traffic("b", 150, 150) })
	m := h.n.msgs
	if len(m) != 1 || strings.Count(m[0], "🔴") != 2 ||
		!strings.Contains(m[0], "abuse_out · 香港（a）\n疑似被利用对外攻击：入站 10.0 Mbps，出站 200.0 Mbps（20.0 倍）") ||
		!strings.Contains(m[0], "busy · b\n入站 150.0 Mbps，出站 150.0 Mbps（阈值 > 100.0 Mbps）") {
		t.Fatalf("net: %q", m)
	}
}

func TestExcludedNodeDoesNotAlert(t *testing.T) {
	h := setup(t, `  - {name: abuse_out, metric: net_out, op: ">=", threshold: 50, ratio: 4, exclude: [b]}
  - {name: offline, metric: offline, for: 1m, exclude: [a]}`)
	h.step(0, func() { h.traffic("a", 1, 200); h.traffic("b", 1, 200) })
	if h.msgs() != 1 || !strings.Contains(h.n.msgs[0], "abuse_out · 香港（a）") || strings.Contains(h.n.msgs[0], "· b") {
		t.Fatalf("exclude: %q", h.n.msgs)
	}
	// After the startup grace both fall silent; only b is covered by offline.
	for i := 0; i < 13; i++ {
		h.step(10*time.Second, nil)
	}
	if h.msgs() != 2 || !strings.Contains(h.n.msgs[1], "🔴 告警 offline · b") || strings.Contains(h.n.msgs[1], "香港") {
		t.Fatalf("offline exclude: %q", h.n.msgs)
	}
}

func TestNetAverageSmoothsSpikes(t *testing.T) {
	h := setup(t, `  - {name: ddos, metric: net_in, op: ">=", threshold: 50, ratio: 4}`)
	// One 10s spike to 200 Mbps among 0s averages to 33 Mbps over a minute.
	for _, in := range []float64{0, 0, 0, 0, 0, 0, 200, 0, 0, 0, 0, 0, 0} {
		h.step(10*time.Second, func() { h.traffic("a", in, 0) })
	}
	if h.msgs() != 0 {
		t.Fatalf("spike alerted: %v", h.n.msgs)
	}
}

func TestOfflineAfterFloodHintsNullRoute(t *testing.T) {
	h := setup(t, `  - {name: offline, metric: offline, for: 1m}
  - {name: ddos, metric: net_in, op: ">=", threshold: 50, ratio: 4, for: 5m, nodes: [a]}`)
	for i := 0; i < 12; i++ {
		h.step(10*time.Second, func() { h.traffic("a", 2, 2); h.traffic("b", 2, 2) })
	}
	// a is flooded for a minute then goes silent; b just goes silent.
	for i := 0; i < 6; i++ {
		h.step(10*time.Second, func() { h.traffic("a", 900, 30) })
	}
	for i := 0; i < 6; i++ {
		h.step(10*time.Second, nil)
	}
	m := strings.Join(h.n.msgs, "\n---\n")
	if !strings.Contains(m, "· 香港（a）\n已 1 分 0 秒 没有上报；停止上报前入站 900.0 Mbps、出站 30.0 Mbps，疑似被攻击后遭商家黑洞") ||
		strings.Count(m, "黑洞") != 1 || strings.Count(m, "🔴 告警 offline") != 2 {
		t.Fatalf("offline: %q", h.n.msgs)
	}
}

const gib = 1 << 30

// periods sets node's periods, newest first, and makes it fresh with the
// newest as its current period.
func (h *harness) periods(node string, ps ...store.Period) {
	h.report(node, 1)
	h.src.status[node].Traffic = &ps[0]
	if h.src.period == nil {
		h.src.period, h.src.daily = map[string][]store.Period{}, map[string][]store.DayTraffic{}
	}
	h.src.period[node] = ps
}

// days fills node's daily usage in the period starting at start: one entry
// per day from first, n days, each rx/tx GiB.
func (h *harness) days(node string, start int64, first string, n int, rx, tx float64) {
	d, _ := time.Parse(time.DateOnly, first)
	var out []store.DayTraffic
	for i := 0; i < n; i++ {
		out = append(out, store.DayTraffic{Day: d.AddDate(0, 0, i).Format(time.DateOnly), RX: int64(rx * gib), TX: int64(tx * gib)})
	}
	h.src.daily[fmt.Sprint(node, "|", start)] = out
}

func TestPeriodReport(t *testing.T) {
	h := setupReports(t, twoNodes, `  - {type: period}`)
	ended := store.Period{Start: sep, End: oct, RX: 30 * gib, TX: 20 * gib}
	h.periods("a", ended)
	h.step(0, nil)
	if h.msgs() != 0 {
		t.Fatalf("first sight reported: %v", h.n.msgs)
	}
	h.days("a", sep, "2026-09-01", 30, 1, 0.5)
	h.src.daily[fmt.Sprint("a|", sep)][14] = store.DayTraffic{Day: "2026-09-15", RX: 4 * gib, TX: 1 * gib}
	next := func(rx int64) store.Period { return store.Period{Start: oct, End: nov, RX: rx} }
	h.step(10*time.Second, func() { h.periods("a", next(1), ended) })
	want := "📊 流量结算 · 香港（a）\n2026-09-01 至 2026-10-01（30 天）：下行 30.00 GB，上行 20.00 GB，合计 50.00 GB\n" +
		"配额 100.00 GB（计费：收+发），用了 50.0%\n日均 1.67 GB，最多的一天 2026-09-15（5.00 GB）\n"
	if h.msgs() != 1 || !strings.HasPrefix(h.n.msgs[0], want) {
		t.Fatalf("report:\n%q\nwant prefix\n%q", h.n.msgs, want)
	}
	ev := h.src.events[len(h.src.events)-1]
	if ev.Event != "report" || ev.Rule != "period" || ev.Target != "2026-09-01" || ev.Value != "50.0%" {
		t.Fatalf("history: %+v", ev)
	}
	// Once per period, also across a restart.
	h.step(10*time.Second, func() { h.periods("a", next(2), ended) })
	h.newEvaluator()
	h.step(10*time.Second, func() { h.periods("a", next(3), ended) })
	if h.msgs() != 1 || len(h.e.Active()) != 0 {
		t.Fatalf("repeated: %v", h.n.msgs)
	}
	// A start going backwards (agent state rebuilt) is only recorded.
	h.step(10*time.Second, func() { h.periods("a", ended) })
	if h.msgs() != 1 {
		t.Fatalf("backwards reported: %v", h.n.msgs)
	}
}

func TestWeeklyReport(t *testing.T) {
	h := setupReports(t, `
  - {id: a, name: 香港, token: `+tokA+`, traffic: {quota_gb: 100}}
  - {id: b, token: `+tokB+`, plan: {expire_at: 2026-10-20}}
  - {id: c, token: cdefghijklmnopqrstuvwxyz0123456789ab}`,
		`  - {type: weekly, at: "mon 09:00"}`)
	sh := h.cfg.Location
	cur := store.Period{Start: oct, End: nov, RX: 8 * gib, TX: 2 * gib}
	h.periods("a", cur, store.Period{Start: sep, End: oct})
	h.days("a", oct, "2026-10-01", 4, 3, 0.5)
	h.days("a", sep, "2026-09-28", 3, 3, 0.5)
	// c's period begins on the 15th at 18:00, and it has reported since 10-03.
	c15 := time.Date(2026, 9, 15, 18, 0, 0, 0, sh)
	h.periods("c", store.Period{Start: c15.Unix(), End: c15.AddDate(0, 1, 0).Unix(), RX: 1500 * gib})
	h.days("c", c15.Unix(), "2026-10-03", 2, 1, 0)
	// Wednesday: Monday's slot is more than a day old, so a fresh deploy
	// doesn't send it.
	h.step(0, nil)
	if h.msgs() != 0 {
		t.Fatalf("stale slot sent: %v", h.n.msgs)
	}
	// Monday 2026-10-05 10:30 in Asia/Shanghai: 90 minutes late, still sent.
	h.now = time.Date(2026, 10, 5, 2, 30, 0, 0, time.UTC)
	h.step(0, func() { h.report("a", 1); h.report("c", 1) })
	// a: 3 + 0.5 GiB a day over 7 whole days; 26.5625 days left to 11-01 00:00.
	// c: 1 GiB a day; 10.3125 days left to 10-15 18:00.
	want := "📊 每周流量\n" +
		"香港（a）：本周期（2026-10-01 起）10.00 GB / 100.00 GB（10.0%），近 7 天 24.50 GB，" +
		"预计周期末 102.97 GB / 100.00 GB（103.0%） ⚠️ 可能超额，11-01 重置\n" +
		"b：暂无流量数据\n" +
		"c：本周期（2026-09-15 18:00 起）1500.00 GB，近 2 天 2.00 GB，预计周期末 1510.31 GB，10-15 18:00 重置\n" +
		"即将到期：b 2026-10-20（还剩 15 天）\n"
	if h.msgs() != 1 || !strings.HasPrefix(h.n.msgs[0], want) {
		t.Fatalf("weekly:\n%q\nwant prefix\n%q", h.n.msgs, want)
	}
	if ev := h.src.events[0]; ev.Node != "" || ev.Rule != "weekly" || ev.Target != "2026-10-05 09:00" || ev.Event != "report" {
		t.Fatalf("history: %+v", ev)
	}
	// Once per slot, also across a restart.
	h.step(time.Hour, nil)
	h.newEvaluator()
	h.step(time.Hour, nil)
	if h.msgs() != 1 || len(h.e.Active()) != 0 {
		t.Fatalf("repeated: %v", h.n.msgs)
	}
}

func TestPacketRates(t *testing.T) {
	h := setup(t, `  - {name: pps_flood, metric: pps_in, op: ">=", threshold: 50000}
  - {name: si, metric: softirq, op: ">", threshold: 30}
  - {name: ddos, metric: net_in, op: ">=", threshold: 50, ratio: 4}`)
	soft := 42.0
	h.step(0, func() {
		// a: a SYN flood, 80k packets/s of 60 bytes: only 38 Mbps.
		h.traffic("a", 38.4, 30)
		h.packets("a", 80000, 79000)
		h.src.status["a"].SoftIRQ = &soft
		// b: a byte flood of large packets, and no softirq reading.
		h.traffic("b", 400, 10)
		h.packets("b", 40000, 1000)
	})
	m := strings.Join(h.n.msgs, "\n")
	if !strings.Contains(m, "🔴 告警 pps_flood · 香港（a）\n入站 8.0 万包/秒（平均 60 字节/包），出站 7.9 万包/秒，软中断 42.0%（阈值 >= 5.0 万包/秒）") ||
		!strings.Contains(m, "🔴 告警 si · 香港（a）\n软中断 42.0%（阈值 > 30.0%）") ||
		!strings.Contains(m, "🔴 告警 ddos · b\n疑似 DDoS：入站 400.0 Mbps，出站 10.0 Mbps（40.0 倍）") ||
		strings.Count(m, "🔴") != 3 {
		t.Fatalf("packets: %q", h.n.msgs)
	}
	// A byte flood shows its packets as evidence.
	h2 := setup(t, `  - {name: ddos, metric: net_in, op: ">=", threshold: 50, ratio: 4}`)
	h2.step(0, func() {
		h2.traffic("a", 800, 40)
		h2.packets("a", 100000, 2000)
		h2.src.status["a"].SoftIRQ = &soft
	})
	if h2.msgs() != 1 || !strings.Contains(h2.n.msgs[0], "\n入站 10.0 万包/秒（平均 1000 字节/包），出站 2000 包/秒，软中断 42.0%\n") {
		t.Fatalf("evidence: %q", h2.n.msgs)
	}
}

// Every metric a rule may name has an implementation, and the page can
// describe every rule and report.
func TestMetricsCoverConfig(t *testing.T) {
	for _, m := range config.Metrics {
		if _, ok := metrics[m]; !ok {
			t.Errorf("metric %s has no implementation", m)
		}
	}
	if len(metrics) != len(config.Metrics) {
		t.Errorf("%d implementations for %d metrics", len(metrics), len(config.Metrics))
	}
	for _, typ := range config.ReportTypes {
		if reports[typ] == nil {
			t.Errorf("report %s has no implementation", typ)
		}
	}
	h := setupConfig(t, "nodes:"+twoNodes+`
alerts:
  - {name: offline, metric: offline, for: 90s, exclude: [b]}
  - {name: ddos, metric: net_in, op: ">=", threshold: 50, ratio: 4, for: 2m, repeat: 1h, notify_recovery: false}
  - {name: disk, metric: disk, op: ">", threshold: 90, nodes: [a]}
reports:
  - {type: traffic_quota, levels: [80, 100]}
  - {type: expiry, days: [1, 7, 0]}
  - {type: weekly, at: "Sun 08:30"}
`)
	want := []RuleView{
		{"offline", "超过 1m30s 没有上报", "全部，除 b", "不重复", "是"},
		{"ddos", "入站 >= 50.0 Mbps，且不低于出站的 4 倍，持续 2m", "全部", "每 1h", "否"},
		{"disk", "磁盘 > 90.0%", "香港", "不重复", "是"},
		{"traffic_quota", "流量达到配额的 80 / 100%（每周期每档一次）", "全部", "", ""},
		{"expiry", "到期前 7 / 1 / 0 天提醒（0 = 当天；每个到期日每档一次）", "全部", "", ""},
		{"weekly", "每周日 08:30 发送流量汇总", "全部", "", ""},
	}
	got := h.e.Rules()
	if len(got) != len(want) {
		t.Fatalf("rules: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("rule %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

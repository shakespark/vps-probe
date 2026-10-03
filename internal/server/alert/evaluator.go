// Package alert decides what is worth a message and sends it.
//
// Two kinds of things are: alert rules, conditions on a metric that fire
// once they have held for a while and recover when they no longer do
// (this file; the metrics are in metrics.go), and reports, which happen
// once (reports.go, traffic.go). Both are evaluated in rounds against what
// the store holds at that moment, and one round sends one merged message.
package alert

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shakespark/vps-probe/internal/server/config"
	"github.com/shakespark/vps-probe/internal/server/notify"
	"github.com/shakespark/vps-probe/internal/server/store"
	"github.com/shakespark/vps-probe/internal/wire"
)

const (
	// EvalEvery is the time between rounds: one per report interval.
	EvalEvery = wire.Interval
	// StartupGrace is how long after the server starts offline isn't
	// judged: every node looks silent until its first report arrives.
	StartupGrace = 2 * time.Minute
	// A firing alert recovers only after its condition has been false for
	// min(for, maxDebounce).
	maxDebounce = time.Minute
	// Rounds run on a ticker, so "held for 20s" may measure 19.99s; half a
	// round of tolerance keeps alerts from firing one round late.
	slack = EvalEvery / 2
)

func held(since, now time.Time, d time.Duration) bool { return now.Sub(since)+slack >= d }

// Source is what the evaluator needs from the store.
type Source interface {
	Status(ctx context.Context, node string) (*store.Status, error)
	Matrix(ctx context.Context, window time.Duration) ([]store.Link, error)
	NetSums(ctx context.Context, node string, from, to int64) ([]store.NetSum, error)
	Periods(ctx context.Context, node string, limit int) ([]store.Period, error)
	Daily(ctx context.Context, node string, start int64) ([]store.DayTraffic, error)
	AlertStates() ([]store.AlertState, []store.ReportMark, error)
	SaveAlerts(store.AlertChanges) error
}

// key identifies one alert: a rule on a node, and for metrics with several
// values per node (mount points, links) the one it is about.
type key struct{ rule, node, target string }

// alert is a rule whose condition holds on a key: pending until it has held
// for the rule's time, then firing.
type alert struct {
	firing     bool
	since      time.Time // the condition has held since
	notified   time.Time // the last message about it
	clearSince time.Time // firing: the condition has been false since (debounce)
	value      float64
}

type markKey struct{ report, node string }

type Evaluator struct {
	cfg     *config.Config
	src     Source
	notify  notify.Notifier
	log     *slog.Logger
	now     func() time.Time
	started time.Time

	mu     sync.Mutex
	alerts map[key]*alert
	marks  map[markKey]store.ReportMark // what each report last sent
}

func New(cfg *config.Config, src Source, n notify.Notifier, log *slog.Logger) *Evaluator {
	return &Evaluator{cfg: cfg, src: src, notify: n, log: log, now: time.Now,
		alerts: map[key]*alert{}, marks: map[markKey]store.ReportMark{}}
}

// Load restores what the previous run stored, and forgets alerts of rules
// and nodes that are no longer configured.
func (e *Evaluator) Load() error {
	e.started = e.now()
	states, marks, err := e.src.AlertStates()
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	var gone []store.AlertState
	for _, s := range states {
		if _, ok := e.cfg.Node(s.Node); !ok || e.rule(s.Rule) == nil {
			gone = append(gone, s)
			continue
		}
		e.alerts[key{s.Rule, s.Node, s.Target}] = &alert{firing: s.Firing, since: time.Unix(s.Since, 0),
			notified: time.Unix(s.Notified, 0), value: s.Value}
	}
	for _, m := range marks {
		e.marks[markKey{m.Report, m.Node}] = m
	}
	return e.src.SaveAlerts(store.AlertChanges{Del: gone})
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

func (e *Evaluator) rule(name string) *config.Rule {
	for i := range e.cfg.Alerts {
		if e.cfg.Alerts[i].Name == name {
			return &e.cfg.Alerts[i]
		}
	}
	return nil
}

// round is one evaluation: the data it looks at, and what it changes and
// sends, collected so that both are applied at once at the end.
type round struct {
	*Evaluator
	ctx     context.Context
	now     time.Time
	snap    *snapshot
	changes store.AlertChanges
	msgs    []string
}

// record adds an event to the history, and its message to this round's
// notification unless silent.
func (r *round) record(k key, event, value, msg string, silent bool) {
	r.changes.Events = append(r.changes.Events, store.AlertEvent{TS: r.now.Unix(), Rule: k.rule, Node: k.node,
		Target: k.target, Event: event, Value: value, Message: msg})
	if !silent {
		r.msgs = append(r.msgs, msg)
	}
}

func (r *round) save(k key, a *alert) {
	r.changes.Put = append(r.changes.Put, store.AlertState{Rule: k.rule, Node: k.node, Target: k.target,
		Firing: a.firing, Since: a.since.Unix(), Notified: a.notified.Unix(), Value: a.value})
}

func (r *round) forget(k key) {
	delete(r.alerts, k)
	r.changes.Del = append(r.changes.Del, store.AlertState{Rule: k.rule, Node: k.node, Target: k.target})
}

// Tick runs one round and sends one merged message for it.
func (e *Evaluator) Tick(ctx context.Context) error {
	now := e.now()
	snap, err := e.snapshot(ctx, now)
	if err != nil {
		return err
	}
	r := &round{Evaluator: e, ctx: ctx, now: now, snap: snap}

	e.mu.Lock()
	for i := range e.cfg.Alerts {
		r.evaluate(&e.cfg.Alerts[i])
	}
	for i := range e.cfg.Reports {
		rep := &e.cfg.Reports[i]
		// A failed query leaves nothing recorded, so the next round retries.
		if err := reports[rep.Type](r, rep); err != nil {
			e.log.Error("report", "type", rep.Type, "err", err)
		}
	}
	e.mu.Unlock()

	if err := e.src.SaveAlerts(r.changes); err != nil {
		e.log.Error("saving alert state", "err", err)
	}
	if len(r.msgs) > 0 {
		e.notify.Notify(strings.Join(r.msgs, "\n\n"))
	}
	return nil
}

// evaluate advances every alert of one rule:
//
//	condition true:   (none) -> pending -> firing, with reminders while firing
//	condition false:  pending -> (none); firing -> (none) after the debounce
//	no observation:   pending -> (none); firing holds
//
// No observation means there is no usable data: the node is offline, the
// link is gone, the server has just started.
func (r *round) evaluate(rule *config.Rule) {
	m := metrics[rule.Metric]
	hold, debounce := time.Duration(rule.For), min(time.Duration(rule.For), maxDebounce)
	if m.timed {
		hold, debounce = 0, 0
	}
	seen := map[key]bool{}
	for i := range r.cfg.Nodes {
		n := &r.cfg.Nodes[i]
		st := r.snap.status[n.ID]
		if !rule.Covers(n.ID) || !m.offline && !st.Online(r.now) {
			continue // stale values say nothing about now
		}
		for _, o := range m.observe(r, rule, n, st) {
			k := key{rule.Name, n.ID, o.target}
			seen[k] = true
			a := r.alerts[k]
			switch {
			case o.cond && a == nil:
				a = &alert{since: r.now, value: o.value}
				if !o.since.IsZero() {
					a.since = o.since
				}
				r.alerts[k] = a
				if hold == 0 {
					r.fire(rule, k, a, o)
				}
			case o.cond && !a.firing:
				a.value = o.value
				if held(a.since, r.now, hold) {
					r.fire(rule, k, a, o)
				}
			case o.cond: // firing
				a.value, a.clearSince = o.value, time.Time{}
				if rule.Repeat > 0 && held(a.notified, r.now, time.Duration(rule.Repeat)) {
					a.notified = r.now
					r.record(k, eventRepeat, m.format(o.value), r.message(titleRepeat, rule, n, o, r.now.Sub(a.since)), false)
					r.save(k, a)
				}
			case a == nil:
			case !a.firing:
				delete(r.alerts, k)
			default: // firing, condition false
				if a.clearSince.IsZero() {
					a.clearSince = r.now
				}
				if held(a.clearSince, r.now, debounce) {
					r.forget(k)
					msg := r.message(titleRecovered, rule, n, o, a.clearSince.Sub(a.since))
					r.record(k, eventRecovered, m.format(o.value), msg, !rule.NotifyRecovery)
				}
			}
		}
	}
	for k, a := range r.alerts {
		if k.rule == rule.Name && !seen[k] && !a.firing {
			delete(r.alerts, k)
		}
	}
}

func (r *round) fire(rule *config.Rule, k key, a *alert, o obs) {
	a.firing, a.notified = true, r.now
	n, _ := r.cfg.Node(k.node)
	r.record(k, eventFiring, metrics[rule.Metric].format(o.value), r.message(titleFiring, rule, n, o, r.now.Sub(a.since)), false)
	r.save(k, a)
}

// Active is an alert currently pending or firing, for the API.
type Active struct {
	Rule   string `json:"rule"`
	Node   string `json:"node"`
	Target string `json:"target"`
	Firing bool   `json:"firing"` // false: pending
	Since  int64  `json:"since"`
	Value  string `json:"value"` // as shown, e.g. "95.2%"
}

// Active lists pending and firing alerts, most recent first.
func (e *Evaluator) Active() []Active {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := []Active{}
	for k, a := range e.alerts {
		out = append(out, Active{Rule: k.rule, Node: k.node, Target: k.target, Firing: a.firing,
			Since: a.since.Unix(), Value: metrics[e.rule(k.rule).Metric].format(a.value)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since > out[j].Since })
	return out
}

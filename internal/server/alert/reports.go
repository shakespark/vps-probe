package alert

import (
	"fmt"
	"math"
	"strconv"

	"github.com/shakespark/vps-probe/internal/server/config"
	"github.com/shakespark/vps-probe/internal/server/store"
)

// reports are the things that are announced once. Each remembers, per node,
// what it last announced (a mark), so that a restart neither repeats nor
// skips one.
var reports = map[string]func(*round, *config.Report) error{
	config.ReportQuota:    (*round).quota,
	config.ReportExpiry:   (*round).expiry,
	config.ReportIPChange: (*round).ipChange,
	config.ReportPeriod:   (*round).period,
	config.ReportWeekly:   (*round).weekly,
}

// last returns what report last announced for node: the thing it was about
// and, for reports with levels, the level reached. ok is false if it never
// announced anything.
func (r *round) last(report, node string) (mark string, level float64, ok bool) {
	m, ok := r.marks[markKey{report, node}]
	return m.Mark, m.Level, ok
}

// remember records what report has now announced for node.
func (r *round) remember(report, node, mark string, level float64) {
	m := store.ReportMark{Report: report, Node: node, Mark: mark, Level: level, At: r.now.Unix()}
	r.marks[markKey{report, node}] = m
	r.changes.Marks = append(r.changes.Marks, m)
}

// announce sends a report's message and adds it to the history.
func (r *round) announce(report, node, target, event, value, msg string) {
	r.record(key{report, node, target}, event, value, msg+"\n"+r.stamp(), false)
}

// covered calls fn for every node the report applies to.
func (r *round) covered(rep *config.Report, fn func(n *config.Node, st *store.Status) error) error {
	for i := range r.cfg.Nodes {
		if n := &r.cfg.Nodes[i]; rep.Covers(n.ID) {
			if err := fn(n, r.snap.status[n.ID]); err != nil {
				return err
			}
		}
	}
	return nil
}

// quota announces when a node's usage crosses a level of its quota: once
// per level per period, and only the highest of several crossed at once.
func (r *round) quota(rep *config.Report) error {
	return r.covered(rep, func(n *config.Node, st *store.Status) error {
		quota := n.Traffic.Quota()
		if quota == 0 || st == nil || st.Traffic == nil {
			return nil
		}
		t := st.Traffic
		period := strconv.FormatInt(t.Start, 10)
		mark, reached, _ := r.last(rep.Type, n.ID)
		if mark != period {
			reached = 0 // a new period starts over
		}
		used := n.Traffic.Billable(t.RX, t.TX)
		pct := 100 * float64(used) / float64(quota)
		level := 0.0
		for _, l := range rep.Levels {
			if pct >= l && l > reached {
				level = l
			}
		}
		if level == 0 {
			return nil
		}
		r.remember(rep.Type, n.ID, period, level)
		r.announce(rep.Type, n.ID, r.day(t.Start), eventNotice, fmt.Sprintf("%.0f%%", level),
			fmt.Sprintf("📊 流量 · %s\n本周期（%s 起）已用 %s，达到配额 %s 的 %.0f%%（计费：%s）", nodeName(n), r.day(t.Start),
				fmtBytes(float64(used)), fmtBytes(float64(quota)), pct, n.Traffic.QuotaModeText()))
		return nil
	})
}

// expiry reminds when a plan's expiry date is a level's number of days
// away: once per level per date, and only the most urgent of several reached
// at once. A renewed or rolled-forward date starts over.
func (r *round) expiry(rep *config.Report) error {
	return r.covered(rep, func(n *config.Node, _ *store.Status) error {
		date, days, ok := n.Plan.Expiry(r.now, r.cfg.Location)
		if !ok {
			return nil
		}
		mark, reached, _ := r.last(rep.Type, n.ID)
		if mark != date {
			reached = math.Inf(1)
		}
		level := math.Inf(1)
		for _, l := range rep.Days {
			if float64(days) <= l && l < reached {
				level = min(level, l)
			}
		}
		if math.IsInf(level, 1) {
			return nil
		}
		r.remember(rep.Type, n.ID, date, level)
		var when string
		switch {
		case days > 0:
			when = fmt.Sprintf("将于 %s 到期，还剩 %d 天", date, days)
		case days == 0:
			when = fmt.Sprintf("今天（%s）到期", date)
		default:
			when = fmt.Sprintf("已于 %s 到期（%d 天前）", date, -days)
		}
		if n.Plan.Price != "" {
			when += "，价格 " + n.Plan.Price
		}
		if n.Plan.RenewMonths > 0 {
			when += fmt.Sprintf("（自动续费，每 %d 个月）", n.Plan.RenewMonths)
		}
		r.announce(rep.Type, n.ID, date, eventNotice, expiryText(days), fmt.Sprintf("⏰ 到期 · %s\n%s", nodeName(n), when))
		return nil
	})
}

func expiryText(days int) string {
	switch {
	case days > 0:
		return fmt.Sprintf("剩 %d 天", days)
	case days == 0:
		return "今天到期"
	}
	return fmt.Sprintf("已过期 %d 天", -days)
}

// ipChange announces when a node's reports start coming from another
// address. The first address seen for a node is only recorded.
func (r *round) ipChange(rep *config.Report) error {
	return r.covered(rep, func(n *config.Node, st *store.Status) error {
		if st == nil || st.IP == "" {
			return nil
		}
		old, _, seen := r.last(rep.Type, n.ID)
		if old == st.IP {
			return nil
		}
		r.remember(rep.Type, n.ID, st.IP, 0)
		if seen {
			r.announce(rep.Type, n.ID, st.IP, eventNotice, "", fmt.Sprintf("🔁 IP 变化 · %s\n%s → %s", nodeName(n), old, st.IP))
		}
		return nil
	})
}

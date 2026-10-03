package alert

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shakespark/vps-probe/internal/server/config"
	"github.com/shakespark/vps-probe/internal/server/store"
)

// period sends a node's totals for a billing period once it has ended,
// which shows as the node's current period being a later one. The first
// period seen for a node is only recorded.
func (r *round) period(rep *config.Report) error {
	return r.covered(rep, func(n *config.Node, st *store.Status) error {
		if st == nil || st.Traffic == nil {
			return nil
		}
		cur := st.Traffic.Start
		mark, _, seen := r.last(rep.Type, n.ID)
		old, _ := strconv.ParseInt(mark, 10, 64)
		if seen && old == cur {
			return nil
		}
		var ended *store.Period
		var days []store.DayTraffic
		if seen && old < cur { // an earlier start: the agent's state was rebuilt or its reset day changed
			ps, err := r.src.Periods(r.ctx, n.ID, 2)
			if err != nil {
				return err
			}
			if len(ps) == 2 && ps[0].Start == cur {
				ended = &ps[1]
				if days, err = r.src.Daily(r.ctx, n.ID, ended.Start); err != nil {
					return err
				}
			}
		}
		r.remember(rep.Type, n.ID, strconv.FormatInt(cur, 10), 0)
		if ended != nil {
			msg, used := r.periodMessage(n, *ended, days)
			r.announce(rep.Type, n.ID, r.day(ended.Start), eventReport, used, msg)
		}
		return nil
	})
}

// periodMessage words an ended period, and returns how much of the quota it
// used, as shown in the history.
func (r *round) periodMessage(n *config.Node, p store.Period, days []store.DayTraffic) (msg, used string) {
	var b strings.Builder
	total := float64(p.RX + p.TX)
	length := int(float64(p.End-p.Start)/86400 + 0.5) // whole days: a day of 23 or 25 hours is still one
	fmt.Fprintf(&b, "📊 流量结算 · %s\n%s 至 %s（%d 天）：下行 %s，上行 %s，合计 %s", nodeName(n), r.day(p.Start), r.day(p.End),
		length, fmtBytes(float64(p.RX)), fmtBytes(float64(p.TX)), fmtBytes(total))
	if quota := n.Traffic.Quota(); quota > 0 {
		used = fmtPct(100 * float64(n.Traffic.Billable(p.RX, p.TX)) / float64(quota))
		fmt.Fprintf(&b, "\n配额 %s（计费：%s），用了 %s", fmtBytes(float64(quota)), n.Traffic.QuotaModeText(), used)
	}
	if length > 0 {
		// Not divided by the number of daily rows: a period that doesn't
		// begin at midnight has a partial day at each end.
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
	return b.String(), used
}

// weekly sends one summary of all covered nodes at the report's weekly
// slot, or up to a day late if the server was down then.
func (r *round) weekly(rep *config.Report) error {
	slot := rep.WeeklySlot(r.now, r.cfg.Location)
	if r.now.Sub(slot) >= 24*time.Hour {
		return nil
	}
	at := slot.Format("2006-01-02 15:04")
	if mark, _, _ := r.last(rep.Type, ""); mark == at {
		return nil
	}
	var b strings.Builder
	b.WriteString("📊 每周流量")
	var soon []string
	err := r.covered(rep, func(n *config.Node, st *store.Status) error {
		line, err := r.weeklyLine(n, st)
		if err != nil {
			return err
		}
		b.WriteString("\n" + line)
		if date, days, ok := n.Plan.Expiry(r.now, r.cfg.Location); ok && days <= 30 {
			left := fmt.Sprintf("还剩 %d 天", days)
			if days < 0 {
				left = fmt.Sprintf("已过期 %d 天", -days)
			}
			soon = append(soon, fmt.Sprintf("%s %s（%s）", nodeName(n), date, left))
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(soon) > 0 {
		b.WriteString("\n即将到期：" + strings.Join(soon, "、"))
	}
	r.remember(rep.Type, "", at, 0)
	r.announce(rep.Type, "", at, eventReport, "", b.String())
	return nil
}

// weeklyLine is one node's line: usage this period, the last 7 whole days,
// and the period's end usage projected at the last 7 days' daily rate.
func (r *round) weeklyLine(n *config.Node, st *store.Status) (string, error) {
	name := nodeName(n)
	if st == nil || st.Traffic == nil {
		return name + "：暂无流量数据", nil
	}
	ps, err := r.src.Periods(r.ctx, n.ID, 2)
	if err != nil {
		return "", err
	}
	if len(ps) == 0 {
		return name + "：暂无流量数据", nil
	}
	cur := ps[0]
	loc := r.cfg.Location
	y, m, d := r.now.In(loc).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, loc)
	from, to := today.AddDate(0, 0, -7).Format(time.DateOnly), today.Format(time.DateOnly)
	var rx7, tx7 int64
	whole := map[string]bool{} // fewer than 7 for a node that started reporting lately
	for _, p := range ps {     // the last 7 days may span the previous period
		days, err := r.src.Daily(r.ctx, n.ID, p.Start)
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

	quota := float64(n.Traffic.Quota())
	used := func(rx, tx float64) string {
		if quota == 0 {
			return fmtBytes(rx + tx)
		}
		v := float64(n.Traffic.Billable(int64(rx), int64(tx)))
		return fmt.Sprintf("%s / %s（%s）", fmtBytes(v), fmtBytes(quota), fmtPct(100*v/quota))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s：本周期（%s 起）%s", name, r.day(cur.Start), used(float64(cur.RX), float64(cur.TX)))
	if len(whole) > 0 {
		fmt.Fprintf(&b, "，近 %d 天 %s", len(whole), fmtBytes(float64(rx7+tx7)))
	}
	end := time.Unix(cur.End, 0).In(loc)
	remain := end.Sub(r.now).Hours() / 24
	if remain <= 0 {
		return b.String(), nil // offline across its reset: no newer period reported yet
	}
	if len(whole) > 0 {
		// rx and tx are projected separately: what a max quota bills is the
		// larger of the two sums.
		k := float64(len(whole))
		prx, ptx := float64(cur.RX)+float64(rx7)/k*remain, float64(cur.TX)+float64(tx7)/k*remain
		fmt.Fprintf(&b, "，预计周期末 %s", used(prx, ptx))
		if quota > 0 && float64(n.Traffic.Billable(int64(prx), int64(ptx))) >= quota {
			b.WriteString(" ⚠️ 可能超额")
		}
	}
	layout := "01-02"
	if end.Hour() != 0 || end.Minute() != 0 {
		layout = "01-02 15:04"
	}
	fmt.Fprintf(&b, "，%s 重置", end.Format(layout))
	return b.String(), nil
}

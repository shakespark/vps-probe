package alert

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/shakespark/vps-probe/internal/server/config"
)

// History events.
const (
	eventFiring    = "firing"
	eventRepeat    = "repeat"
	eventRecovered = "recovered"
	eventNotice    = "notice" // a report about one thing: a quota level, an expiry, an address
	eventReport    = "report" // a traffic summary
)

// Events lists every history event, for the API's filter.
var Events = []string{eventFiring, eventRepeat, eventRecovered, eventNotice, eventReport}

const (
	titleFiring    = "🔴 告警"
	titleRepeat    = "🟠 仍在告警"
	titleRecovered = "🟢 恢复"
)

// message words an alert: what it is about, the observation against the
// rule's threshold, how long it has lasted, the supporting evidence and the
// time.
func (r *round) message(title string, rule *config.Rule, n *config.Node, o obs, lasted time.Duration) string {
	m := metrics[rule.Metric]
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s · %s\n%s", title, rule.Name, nodeName(n), o.detail)
	if title == titleRecovered {
		fmt.Fprintf(&b, "，%s %s", m.over, fmtDur(lasted))
	} else {
		if rule.Op != "" {
			b.WriteString("（" + thresholdText(rule) + "）")
		}
		if lasted > 0 && !m.timed {
			b.WriteString("，已持续 " + fmtDur(lasted))
		}
	}
	if o.evidence != "" {
		b.WriteString("\n" + strings.TrimRight(o.evidence, "\n"))
	}
	b.WriteString("\n" + r.stamp())
	return b.String()
}

// thresholdText is a rule's condition without the metric's name:
// "阈值 >= 50.0 Mbps，且不低于出站的 4 倍".
func thresholdText(rule *config.Rule) string {
	s := fmt.Sprintf("阈值 %s %s", rule.Op, metrics[rule.Metric].format(rule.Threshold))
	if rule.Ratio > 0 {
		other := "入站"
		if inbound(rule.Metric) {
			other = "出站"
		}
		s += fmt.Sprintf("，且不低于%s的 %g 倍", other, rule.Ratio)
	}
	return s
}

// stamp is the last line of every message: when it was written, in the
// server's timezone.
func (r *round) stamp() string { return r.now.In(r.cfg.Location).Format("2006-01-02 15:04:05") }

// nodeName is how messages name a node: its name, and its id when that
// differs.
func nodeName(n *config.Node) string {
	if n.Name != n.ID {
		return fmt.Sprintf("%s（%s）", n.Name, n.ID)
	}
	return n.ID
}

// day formats a period boundary as a date in the server's timezone, with
// the time when periods don't begin at midnight.
func (e *Evaluator) day(unix int64) string {
	t := time.Unix(unix, 0).In(e.cfg.Location)
	if t.Hour() != 0 || t.Minute() != 0 {
		return t.Format("2006-01-02 15:04")
	}
	return t.Format(time.DateOnly)
}

func fmtPct(v float64) string  { return fmt.Sprintf("%.1f%%", v) }
func fmtLoad(v float64) string { return fmt.Sprintf("%.2f", v) }
func fmtMs(v float64) string   { return fmt.Sprintf("%.1f ms", v) }
func fmtMbps(v float64) string { return fmt.Sprintf("%.1f Mbps", v) }

func fmtPPS(v float64) string {
	if v >= 1e4 {
		return fmt.Sprintf("%.1f 万包/秒", v/1e4)
	}
	return fmt.Sprintf("%.0f 包/秒", v)
}

// fmtSilence is the offline metric's value: seconds without a report.
func fmtSilence(v float64) string { return fmtDur(time.Duration(v) * time.Second) }

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

package alert

import (
	"fmt"
	"slices"
	"strings"

	"github.com/shakespark/vps-probe/internal/server/config"
)

// RuleView is one configured rule or report in words, for the alerts page.
type RuleView struct {
	Name      string `json:"name"`
	Condition string `json:"condition"`
	Nodes     string `json:"nodes"`
	Repeat    string `json:"repeat"`   // "" for reports
	Recovery  string `json:"recovery"` // "" for reports
}

// Rules describes the alert rules and reports in effect, in config order.
func (e *Evaluator) Rules() []RuleView {
	out := []RuleView{}
	for i := range e.cfg.Alerts {
		r := &e.cfg.Alerts[i]
		v := RuleView{Name: r.Name, Condition: condition(r), Nodes: e.scope(r.Scope), Repeat: "不重复", Recovery: "是"}
		if r.Repeat > 0 {
			v.Repeat = "每 " + r.Repeat.String()
		}
		if !r.NotifyRecovery {
			v.Recovery = "否"
		}
		out = append(out, v)
	}
	for i := range e.cfg.Reports {
		r := &e.cfg.Reports[i]
		out = append(out, RuleView{Name: r.Type, Condition: occasion(r), Nodes: e.scope(r.Scope)})
	}
	return out
}

func condition(r *config.Rule) string {
	if r.Metric == config.MetricOffline {
		return fmt.Sprintf("超过 %s 没有上报", r.For)
	}
	m := metrics[r.Metric]
	s := fmt.Sprintf("%s %s %s", m.label, r.Op, m.format(r.Threshold))
	if _, rest, ok := strings.Cut(thresholdText(r), "，"); ok {
		s += "，" + rest
	}
	if r.For > 0 {
		s += "，持续 " + r.For.String()
	}
	return s
}

var weekdayNames = map[string]string{"Mon": "一", "Tue": "二", "Wed": "三", "Thu": "四", "Fri": "五", "Sat": "六", "Sun": "日"}

func occasion(r *config.Report) string {
	join := func(vs []float64) string {
		s := make([]string, len(vs))
		for i, v := range vs {
			s[i] = fmt.Sprintf("%g", v)
		}
		return strings.Join(s, " / ")
	}
	switch r.Type {
	case config.ReportQuota:
		return fmt.Sprintf("流量达到配额的 %s%%（每周期每档一次）", join(r.Levels))
	case config.ReportExpiry:
		days := slices.DeleteFunc(slices.Clone(r.Days), func(d float64) bool { return d == 0 })
		slices.Sort(days)
		slices.Reverse(days)
		s := "到期当天提醒"
		if len(days) > 0 {
			s = fmt.Sprintf("到期前 %s 天", join(days))
			if len(days) < len(r.Days) {
				s += "和当天"
			}
			s += "提醒"
		}
		return s + "（每个到期日每档一次）"
	case config.ReportIPChange:
		return "上报来源 IP 变化时通知"
	case config.ReportPeriod:
		return "每个流量周期结束时发送结算"
	}
	day, at, _ := strings.Cut(r.At, " ")
	return fmt.Sprintf("每周%s %s 发送流量汇总", weekdayNames[day], at)
}

// scope names the nodes a rule covers.
func (e *Evaluator) scope(s config.Scope) string {
	names := func(ids []string) string {
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = id
			if n, ok := e.cfg.Node(id); ok {
				out[i] = n.Name
			}
		}
		return strings.Join(out, "、")
	}
	switch {
	case s.Nodes.IDs != nil:
		return names(s.Nodes.IDs)
	case len(s.Exclude) > 0:
		return "全部，除 " + names(s.Exclude)
	}
	return "全部"
}

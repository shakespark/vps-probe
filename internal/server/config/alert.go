package config

import (
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/shakespark/vps-probe/internal/wire"
)

// Rule is one alert: a condition on a metric that fires once it has held
// for a while and recovers when it no longer does. What each metric measures
// is defined in package alert.
type Rule struct {
	Name   string   `yaml:"name"`
	Metric string   `yaml:"metric"`
	Op     string   `yaml:"op"` // > | >= | < | <=
	For    Duration `yaml:"for"`
	Repeat Duration `yaml:"repeat"` // remind this often while firing; 0 = never
	Scope  `yaml:",inline"`

	Threshold      float64 `yaml:"-"` // in the metric's unit
	NotifyRecovery bool    `yaml:"-"` // send a message on recovery too; the default
	// Rate metrics only: also require this direction to be at least Ratio
	// times the other one, so symmetric relay traffic doesn't fire. 0 = no
	// such requirement.
	Ratio float64 `yaml:"ratio"`

	hasThreshold bool
}

// Metrics a rule can watch.
const (
	MetricOffline  = "offline" // no report for the rule's `for`; takes no op or threshold
	MetricCPU      = "cpu"     // percent
	MetricSteal    = "steal"
	MetricSoftIRQ  = "softirq"
	MetricLoad1    = "load1"
	MetricMem      = "mem"       // percent
	MetricSwap     = "swap"      // percent
	MetricDisk     = "disk"      // percent, each mount point on its own
	MetricPingLoss = "ping_loss" // percent, each link on its own
	MetricPingAvg  = "ping_avg"  // milliseconds
	MetricNetIn    = "net_in"    // Mbps, the node's interfaces together
	MetricNetOut   = "net_out"
	MetricPPSIn    = "pps_in" // packets per second
	MetricPPSOut   = "pps_out"
)

var (
	Metrics = []string{MetricOffline, MetricCPU, MetricSteal, MetricSoftIRQ, MetricLoad1, MetricMem, MetricSwap,
		MetricDisk, MetricPingLoss, MetricPingAvg, MetricNetIn, MetricNetOut, MetricPPSIn, MetricPPSOut}
	// rateMetrics measure one direction of traffic and may take a ratio.
	rateMetrics = []string{MetricNetIn, MetricNetOut, MetricPPSIn, MetricPPSOut}
	ops         = []string{">", ">=", "<", "<="}
)

// UnmarshalYAML tells an omitted threshold or notify_recovery from a zero
// one, which the plain fields cannot.
func (r *Rule) UnmarshalYAML(n *yaml.Node) error {
	type plain Rule // the fields without this method
	raw := struct {
		*plain         `yaml:",inline"`
		Threshold      *float64 `yaml:"threshold"`
		NotifyRecovery *bool    `yaml:"notify_recovery"`
	}{plain: (*plain)(r)}
	if err := decodeStrict(n, &raw); err != nil {
		return err
	}
	if r.hasThreshold = raw.Threshold != nil; r.hasThreshold {
		r.Threshold = *raw.Threshold
	}
	r.NotifyRecovery = raw.NotifyRecovery == nil || *raw.NotifyRecovery
	return nil
}

func threshold(name, metric, op string, v float64, held time.Duration) Rule {
	return Rule{Name: name, Metric: metric, Op: op, Threshold: v, hasThreshold: true, For: Duration(held), NotifyRecovery: true}
}

// DefaultRules apply when the config has no alerts key.
func DefaultRules() []Rule {
	ddos := threshold("ddos", MetricNetIn, ">=", 50, 2*time.Minute)
	abuse := threshold("abuse_out", MetricNetOut, ">=", 50, 5*time.Minute)
	ddos.Ratio, abuse.Ratio = 4, 4
	return []Rule{
		{Name: "offline", Metric: MetricOffline, For: Duration(time.Minute), NotifyRecovery: true},
		threshold("cpu_high", MetricCPU, ">", 90, 5*time.Minute),
		threshold("mem_high", MetricMem, ">", 90, 5*time.Minute),
		threshold("disk_full", MetricDisk, ">", 90, 10*time.Minute),
		threshold("link_loss", MetricPingLoss, ">", 20, 3*time.Minute),
		ddos,
		abuse,
	}
}

func (c *Config) validateAlerts(p *problems, ids map[string]bool) {
	names := map[string]bool{}
	for i := range c.Alerts {
		r := &c.Alerts[i]
		where := "alerts[" + r.Name + "]"
		switch {
		case r.Name == "" || len(r.Name) > 64:
			p.add("alerts[%d].name: required, at most 64 characters", i)
		case names[r.Name]:
			p.add("%s: duplicate name", where)
		case slices.Contains(ReportTypes, r.Name):
			p.add("%s: %q is the name of a report; choose another", where, r.Name)
		}
		names[r.Name] = true
		if !slices.Contains(Metrics, r.Metric) {
			p.add("%s: metric %q: want one of %v", where, r.Metric, Metrics)
			continue
		}
		r.Scope.validate(p, where, ids)
		if r.For < 0 || r.Repeat < 0 {
			p.add("%s: for/repeat must not be negative", where)
		}
		if r.Repeat > 0 && time.Duration(r.Repeat) < time.Minute {
			p.add("%s: repeat: at least 1m, or omit it for no reminders", where)
		}
		if r.Metric == MetricOffline {
			if r.Op != "" || r.hasThreshold {
				p.add("%s: offline takes only for (how long without reports)", where)
			}
			if time.Duration(r.For) < wire.OfflineAfter {
				p.add("%s: offline needs for >= %s, when the page shows a node as offline", where, Duration(wire.OfflineAfter))
			}
		} else {
			if !slices.Contains(ops, r.Op) {
				p.add("%s: op %q: want one of %v", where, r.Op, ops)
			}
			if !r.hasThreshold {
				p.add("%s: threshold is required", where)
			}
		}
		if r.Ratio != 0 {
			switch {
			case !slices.Contains(rateMetrics, r.Metric):
				p.add("%s: ratio only applies to %v", where, rateMetrics)
			case r.Ratio < 1:
				p.add("%s: ratio: want >= 1 (this direction at least ratio x the other), or omit it", where)
			case r.Op != ">" && r.Op != ">=":
				p.add("%s: ratio needs op > or >=", where)
			}
		}
	}
}

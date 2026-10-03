package config

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Report is a message sent when something happens once, not a condition
// that fires and recovers. There is at most one report of each type.
type Report struct {
	Type  string `yaml:"type"`
	Scope `yaml:",inline"`

	Levels []float64 `yaml:"levels"` // traffic_quota: percent of the quota, ascending
	Days   []float64 `yaml:"days"`   // expiry: whole days before the date; 0 = on the day
	At     string    `yaml:"at"`     // weekly: "Mon 09:00" in the server timezone

	atDay time.Weekday
	atMin int // minutes after midnight
}

// Report types.
const (
	ReportQuota    = "traffic_quota" // usage crosses a level of the quota; once per level per period
	ReportExpiry   = "expiry"        // a plan's expire_at is days away; once per day level per date
	ReportIPChange = "ip_change"     // a node's reports start coming from another address
	ReportPeriod   = "period"        // a node's billing period ended: its totals
	ReportWeekly   = "weekly"        // every node's traffic, once a week
)

var ReportTypes = []string{ReportQuota, ReportExpiry, ReportIPChange, ReportPeriod, ReportWeekly}

// DefaultReports apply when the config has no reports key. ip_change is not
// among them: nodes with a dynamic address would send it all the time.
func DefaultReports() []Report {
	return []Report{
		{Type: ReportQuota, Levels: []float64{80, 90, 100}},
		{Type: ReportExpiry, Days: []float64{7, 1}},
		{Type: ReportPeriod},
		{Type: ReportWeekly, At: "Mon 09:00"},
	}
}

func (c *Config) validateReports(p *problems, ids map[string]bool) {
	seen := map[string]bool{}
	for i := range c.Reports {
		r := &c.Reports[i]
		where := "reports[" + r.Type + "]"
		if !slices.Contains(ReportTypes, r.Type) {
			p.add("reports[%d].type %q: want one of %v", i, r.Type, ReportTypes)
			continue
		}
		if seen[r.Type] {
			p.add("%s: listed twice", where)
		}
		seen[r.Type] = true
		r.Scope.validate(p, where, ids)

		if r.Type == ReportQuota {
			if len(r.Levels) == 0 || !slices.IsSorted(r.Levels) || r.Levels[0] <= 0 || r.Levels[len(r.Levels)-1] > 1000 {
				p.add("%s: levels: want ascending percentages, e.g. [80, 90, 100]", where)
			}
		} else if r.Levels != nil {
			p.add("%s: levels only apply to traffic_quota", where)
		}
		if r.Type == ReportExpiry {
			if len(r.Days) == 0 || slices.ContainsFunc(r.Days, func(d float64) bool { return d < 0 || d > 365 || d != math.Trunc(d) }) ||
				len(slices.Compact(slices.Sorted(slices.Values(r.Days)))) != len(r.Days) {
				p.add("%s: days: want distinct whole days 0-365 before the date, e.g. [7, 1] (0 = on the day)", where)
			}
		} else if r.Days != nil {
			p.add("%s: days only apply to expiry", where)
		}
		if r.Type == ReportWeekly {
			d, m, ok := parseAt(r.At)
			if !ok {
				p.add("%s: at %q: want a weekday and a time, e.g. \"Mon 09:00\"", where, r.At)
			}
			r.atDay, r.atMin = d, m
			r.At = fmt.Sprintf("%s %02d:%02d", weekdays[d], m/60, m%60)
		} else if r.At != "" {
			p.add("%s: at only applies to weekly", where)
		}
	}
}

var weekdays = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

// parseAt reads "Mon 09:00".
func parseAt(s string) (time.Weekday, int, bool) {
	day, hm, ok := strings.Cut(strings.TrimSpace(s), " ")
	if !ok {
		return 0, 0, false
	}
	i := slices.IndexFunc(weekdays, func(w string) bool { return strings.EqualFold(w, day) })
	t, err := time.Parse("15:04", strings.TrimSpace(hm))
	if i < 0 || err != nil {
		return 0, 0, false
	}
	return time.Weekday(i), t.Hour()*60 + t.Minute(), true
}

// WeeklySlot returns the newest send time of a weekly report at or before
// now.
func (r *Report) WeeklySlot(now time.Time, loc *time.Location) time.Time {
	now = now.In(loc)
	y, m, d := now.Date()
	back := (int(now.Weekday()) - int(r.atDay) + 7) % 7
	slot := time.Date(y, m, d-back, r.atMin/60, r.atMin%60, 0, 0, loc)
	if slot.After(now) {
		slot = time.Date(y, m, d-back-7, r.atMin/60, r.atMin%60, 0, 0, loc)
	}
	return slot
}

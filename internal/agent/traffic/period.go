package traffic

import "time"

const dateLayout = "2006-01-02"

// Reset is when billing periods begin: Hour:Minute on Day of each month, in
// local time. When a month is shorter than Day, its last day is used.
type Reset struct {
	Day, Hour, Minute int
}

// PeriodStart returns the start of the billing period containing t.
func PeriodStart(t time.Time, loc *time.Location, r Reset) time.Time {
	t = t.In(loc)
	y, m, _ := t.Date()
	start := anchor(y, m, r, loc)
	if t.Before(start) {
		start = anchor(y, m-1, r, loc)
	}
	return start
}

// PrevPeriodStart returns the start of the period before the one beginning at
// start.
func PrevPeriodStart(start time.Time, loc *time.Location, r Reset) time.Time {
	return PeriodStart(start.Add(-time.Nanosecond), loc, r)
}

// NextPeriodStart returns the start of the period after the one beginning
// at start. Periods last 28 to 31 days, so 32 days on is always inside the
// next one.
func NextPeriodStart(start time.Time, loc *time.Location, r Reset) time.Time {
	return PeriodStart(start.In(loc).AddDate(0, 0, 32), loc, r)
}

func anchor(y int, m time.Month, r Reset, loc *time.Location) time.Time {
	// time.Date normalizes month overflow, so m-1 in January is fine.
	first := time.Date(y, m, 1, 0, 0, 0, 0, loc)
	last := first.AddDate(0, 1, -1).Day()
	d := min(r.Day, last)
	return time.Date(first.Year(), first.Month(), d, r.Hour, r.Minute, 0, 0, loc)
}

// PeriodKey formats a period start as its key in the state file. There is
// one period per month, so the start date alone is unique.
func PeriodKey(start time.Time) string {
	return start.Format(dateLayout)
}

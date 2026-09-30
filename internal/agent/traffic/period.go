package traffic

import "time"

const dateLayout = "2006-01-02"

// PeriodStart returns the start of the billing period containing t. Periods
// begin at 00:00 on resetDay of each month in loc; when a month is shorter
// than resetDay, its last day is used instead.
func PeriodStart(t time.Time, loc *time.Location, resetDay int) time.Time {
	t = t.In(loc)
	y, m, _ := t.Date()
	start := anchor(y, m, resetDay, loc)
	if t.Before(start) {
		start = anchor(y, m-1, resetDay, loc)
	}
	return start
}

// PrevPeriodStart returns the start of the period before the one beginning at
// start.
func PrevPeriodStart(start time.Time, loc *time.Location, resetDay int) time.Time {
	return PeriodStart(start.Add(-time.Nanosecond), loc, resetDay)
}

func anchor(y int, m time.Month, resetDay int, loc *time.Location) time.Time {
	// time.Date normalizes month overflow, so m-1 in January is fine.
	first := time.Date(y, m, 1, 0, 0, 0, 0, loc)
	last := first.AddDate(0, 1, -1).Day()
	d := min(resetDay, last)
	return time.Date(first.Year(), first.Month(), d, 0, 0, 0, 0, loc)
}

// PeriodKey formats a period start for use as a map key and on the wire.
func PeriodKey(start time.Time) string {
	return start.Format(dateLayout)
}

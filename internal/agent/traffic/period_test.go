package traffic

import (
	"testing"
	"time"
)

func TestPeriodStart(t *testing.T) {
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	ts := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04:05", s, sh)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		at   string
		day  int
		want string
	}{
		{"2026-09-30 13:00:00", 1, "2026-09-01"},
		{"2026-09-01 00:00:00", 1, "2026-09-01"},
		{"2026-08-31 23:59:59", 1, "2026-08-01"},
		{"2026-01-15 10:00:00", 1, "2026-01-01"},
		{"2026-09-14 23:59:59", 15, "2026-08-15"},
		{"2026-09-15 00:00:00", 15, "2026-09-15"},
		{"2026-01-10 00:00:00", 15, "2025-12-15"}, // crosses year
		// reset_day beyond month length clamps to the last day
		{"2026-02-28 00:00:00", 31, "2026-02-28"},
		{"2026-02-27 23:59:59", 31, "2026-01-31"},
		{"2026-03-30 12:00:00", 31, "2026-02-28"},
		{"2026-03-31 00:00:00", 31, "2026-03-31"},
		{"2028-02-29 00:00:00", 30, "2028-02-29"}, // leap year
		{"2028-02-28 23:00:00", 30, "2028-01-30"},
		{"2026-04-30 00:00:00", 31, "2026-04-30"},
	}
	for _, c := range cases {
		got := PeriodKey(PeriodStart(ts(c.at), sh, Reset{Day: c.day}))
		if got != c.want {
			t.Errorf("PeriodStart(%s, day=%d) = %s, want %s", c.at, c.day, got, c.want)
		}
	}
}

func TestPeriodStartWithTime(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	r := Reset{Day: 21, Hour: 18, Minute: 21}
	cases := []struct{ at, want string }{
		{"2026-10-01 10:00:00", "2026-09-21 18:21"},
		{"2026-10-21 18:20:59", "2026-09-21 18:21"},
		{"2026-10-21 18:21:00", "2026-10-21 18:21"},
		{"2026-10-22 00:00:00", "2026-10-21 18:21"},
	}
	for _, c := range cases {
		at, _ := time.ParseInLocation("2006-01-02 15:04:05", c.at, sh)
		if got := PeriodStart(at, sh, r).Format("2006-01-02 15:04"); got != c.want {
			t.Errorf("PeriodStart(%s) = %s, want %s", c.at, got, c.want)
		}
	}
	// Clamped to the month's last day, at the same time of day.
	feb, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-03-01 00:00:00", sh)
	if got := PeriodStart(feb, sh, Reset{Day: 31, Hour: 6}).Format("2006-01-02 15:04"); got != "2026-02-28 06:00" {
		t.Errorf("clamp: got %s", got)
	}
	start, _ := time.ParseInLocation("2006-01-02 15:04", "2026-10-21 18:21", sh)
	if got := PeriodKey(PrevPeriodStart(start, sh, r)); got != "2026-09-21" {
		t.Errorf("prev: got %s", got)
	}
}

func TestPeriodStartUsesLocation(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	// 2026-08-31 17:00 UTC is 2026-09-01 01:00 in Shanghai.
	at := time.Date(2026, 8, 31, 17, 0, 0, 0, time.UTC)
	if got := PeriodKey(PeriodStart(at, sh, Reset{Day: 1})); got != "2026-09-01" {
		t.Fatalf("Shanghai: got %s", got)
	}
	if got := PeriodKey(PeriodStart(at, time.UTC, Reset{Day: 1})); got != "2026-08-01" {
		t.Fatalf("UTC: got %s", got)
	}
}

func TestPeriodStartDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	// DST starts 2026-03-08; periods still begin at local midnight.
	at := time.Date(2026, 3, 20, 12, 0, 0, 0, ny)
	start := PeriodStart(at, ny, Reset{Day: 8})
	if start.Hour() != 0 || PeriodKey(start) != "2026-03-08" {
		t.Fatalf("got %v", start)
	}
	prev := PrevPeriodStart(start, ny, Reset{Day: 8})
	if PeriodKey(prev) != "2026-02-08" {
		t.Fatalf("prev = %v", prev)
	}
}

func TestPrevPeriodStart(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	cases := []struct {
		start string
		day   int
		want  string
	}{
		{"2026-09-01", 1, "2026-08-01"},
		{"2026-01-01", 1, "2025-12-01"},
		{"2026-03-31", 31, "2026-02-28"},
		{"2026-02-28", 31, "2026-01-31"},
	}
	for _, c := range cases {
		s, _ := time.ParseInLocation(dateLayout, c.start, sh)
		if got := PeriodKey(PrevPeriodStart(s, sh, Reset{Day: c.day})); got != c.want {
			t.Errorf("Prev(%s, %d) = %s, want %s", c.start, c.day, got, c.want)
		}
	}
}

func TestNextPeriodStart(t *testing.T) {
	sh, _ := time.LoadLocation("Asia/Shanghai")
	cases := []struct {
		start string
		r     Reset
		want  string
	}{
		{"2026-09-01 00:00", Reset{Day: 1}, "2026-10-01 00:00"},
		{"2026-12-01 00:00", Reset{Day: 1}, "2027-01-01 00:00"},
		{"2026-09-21 18:21", Reset{Day: 21, Hour: 18, Minute: 21}, "2026-10-21 18:21"},
		{"2026-01-31 00:00", Reset{Day: 31}, "2026-02-28 00:00"},
		{"2026-02-28 00:00", Reset{Day: 31}, "2026-03-31 00:00"},
		{"2026-01-30 00:00", Reset{Day: 30}, "2026-02-28 00:00"},
		{"2026-02-28 00:00", Reset{Day: 30}, "2026-03-30 00:00"},
	}
	for _, c := range cases {
		start, _ := time.ParseInLocation("2006-01-02 15:04", c.start, sh)
		if got := NextPeriodStart(start, sh, c.r).Format("2006-01-02 15:04"); got != c.want {
			t.Errorf("NextPeriodStart(%s, %+v) = %s, want %s", c.start, c.r, got, c.want)
		}
	}
}

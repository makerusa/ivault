package schedule

import (
	"testing"
	"time"
)

type fakeDB map[string]string

func (f fakeDB) GetConfig(k string) (string, error) { return f[k], nil }

func at(weekday time.Weekday, h, m int) time.Time {
	// 2026-07-12 is a Sunday; offset to the requested weekday.
	base := time.Date(2026, 7, 12, h, m, 0, 0, time.UTC)
	return base.AddDate(0, 0, int(weekday))
}

func TestMatchesTime(t *testing.T) {
	s := Load(fakeDB{
		"sched_enabled": "true",
		"sched_days":    "sun,tue,fri",
		"sched_time":    "03:00",
	})
	if !s.MatchesTime(at(time.Sunday, 3, 0)) {
		t.Error("should fire Sunday 03:00")
	}
	if !s.MatchesTime(at(time.Friday, 3, 0)) {
		t.Error("should fire Friday 03:00")
	}
	if s.MatchesTime(at(time.Monday, 3, 0)) {
		t.Error("should NOT fire Monday (not selected)")
	}
	if s.MatchesTime(at(time.Sunday, 3, 1)) {
		t.Error("should NOT fire at 03:01")
	}
	if s.MatchesTime(at(time.Sunday, 4, 0)) {
		t.Error("should NOT fire at 04:00")
	}

	// Disabled schedule never fires.
	if Load(fakeDB{"sched_days": "sun", "sched_time": "03:00"}).MatchesTime(at(time.Sunday, 3, 0)) {
		t.Error("disabled schedule should not fire")
	}
}

func TestInBlackout(t *testing.T) {
	// Same-day window 09:00–17:00.
	day := Load(fakeDB{
		"sched_blackout_enabled": "true",
		"sched_blackout_from":    "09:00",
		"sched_blackout_to":      "17:00",
	})
	if !day.InBlackout(at(time.Monday, 12, 0)) {
		t.Error("12:00 should be in 09:00–17:00 blackout")
	}
	if day.InBlackout(at(time.Monday, 8, 0)) || day.InBlackout(at(time.Monday, 17, 0)) {
		t.Error("08:00 and 17:00 should be outside 09:00–17:00")
	}

	// Wrapping window 22:00–06:00.
	night := Load(fakeDB{
		"sched_blackout_enabled": "true",
		"sched_blackout_from":    "22:00",
		"sched_blackout_to":      "06:00",
	})
	if !night.InBlackout(at(time.Monday, 23, 30)) || !night.InBlackout(at(time.Monday, 2, 0)) {
		t.Error("23:30 and 02:00 should be in wrapping 22:00–06:00 blackout")
	}
	if night.InBlackout(at(time.Monday, 12, 0)) {
		t.Error("12:00 should be outside 22:00–06:00")
	}

	// Disabled blackout is never active.
	if Load(fakeDB{"sched_blackout_from": "09:00", "sched_blackout_to": "17:00"}).InBlackout(at(time.Monday, 12, 0)) {
		t.Error("disabled blackout should not be active")
	}
}

func TestTimezone(t *testing.T) {
	// A schedule set for 03:00 in a UTC-5 zone should fire at 08:00 UTC, not
	// 03:00 UTC — the wall-clock comparison happens in the configured zone.
	s := Load(fakeDB{
		"sched_enabled":  "true",
		"sched_days":     "mon",
		"sched_time":     "03:00",
		"sched_timezone": "America/New_York", // EST = UTC-5 in January
	})
	// 2026-01-05 is a Monday. 08:00 UTC == 03:00 EST.
	utc0800 := time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)
	if !s.MatchesTime(utc0800) {
		t.Error("03:00 America/New_York schedule should match 08:00 UTC")
	}
	utc0300 := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	if s.MatchesTime(utc0300) {
		t.Error("should NOT match 03:00 UTC when the zone is America/New_York")
	}

	// An invalid/absent timezone falls back to UTC.
	if loc := Load(fakeDB{"sched_timezone": "Nowhere/Bogus"}).loc(); loc != time.UTC {
		t.Errorf("invalid timezone should fall back to UTC, got %v", loc)
	}
}

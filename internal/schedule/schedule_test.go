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

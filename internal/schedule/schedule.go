// Package schedule holds the device's maintenance schedule: which days/time a
// sync fires, and a blackout window during which sync is blocked entirely
// (including manual triggers). The schedule is delivered by the portal via the
// heartbeat and persisted in the config table, so it survives restarts and can
// change at runtime.
package schedule

import (
	"strconv"
	"strings"
	"time"
)

// DB is the minimal config-table accessor the schedule needs.
type DB interface {
	GetConfig(key string) (string, error)
}

// Config-table keys the heartbeat persists.
const (
	keyEnabled         = "sched_enabled"
	keyDays            = "sched_days" // CSV of weekday abbrevs: sun,mon,tue,wed,thu,fri,sat
	keyTime            = "sched_time" // "HH:MM"
	keyBlackoutEnabled = "sched_blackout_enabled"
	keyBlackoutFrom    = "sched_blackout_from" // "HH:MM"
	keyBlackoutTo      = "sched_blackout_to"   // "HH:MM"
	keyTimezone        = "sched_timezone"      // IANA tz; times are evaluated here
)

// Schedule is the effective schedule + blackout for the device.
type Schedule struct {
	Enabled bool
	Days    map[time.Weekday]bool
	Hour    int
	Min     int

	BlackoutEnabled bool
	BlackoutFrom    int // minutes since midnight
	BlackoutTo      int // minutes since midnight

	// Loc is the timezone the schedule is evaluated in (the user's account
	// timezone, pushed by the portal). Times passed to MatchesTime/InBlackout
	// are converted to this zone before the wall-clock comparison, so the
	// schedule fires in the user's local time regardless of the system clock's
	// zone. Defaults to UTC.
	Loc *time.Location
}

var weekdayAbbr = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
	"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// Load reads the schedule from the config table. Missing/invalid values yield a
// disabled schedule (manual-only), which is the safe default.
func Load(db DB) Schedule {
	s := Schedule{Days: map[time.Weekday]bool{}, Loc: time.UTC}
	if v, err := db.GetConfig(keyTimezone); err == nil && v != "" {
		if loc, e := time.LoadLocation(v); e == nil {
			s.Loc = loc
		}
	}
	if v, err := db.GetConfig(keyEnabled); err == nil {
		s.Enabled = v == "true" || v == "1"
	}
	if v, err := db.GetConfig(keyDays); err == nil && v != "" {
		for _, d := range strings.Split(v, ",") {
			if wd, ok := weekdayAbbr[strings.ToLower(strings.TrimSpace(d))]; ok {
				s.Days[wd] = true
			}
		}
	}
	if v, err := db.GetConfig(keyTime); err == nil {
		s.Hour, s.Min = parseHHMM(v)
	}
	if v, err := db.GetConfig(keyBlackoutEnabled); err == nil {
		s.BlackoutEnabled = v == "true" || v == "1"
	}
	if v, err := db.GetConfig(keyBlackoutFrom); err == nil {
		h, m := parseHHMM(v)
		s.BlackoutFrom = h*60 + m
	}
	if v, err := db.GetConfig(keyBlackoutTo); err == nil {
		h, m := parseHHMM(v)
		s.BlackoutTo = h*60 + m
	}
	return s
}

// MatchesTime reports whether a scheduled sync should fire at now — the
// schedule is enabled, now's weekday is selected, and now's HH:MM equals the
// scheduled time (minute granularity).
func (s Schedule) MatchesTime(now time.Time) bool {
	if !s.Enabled || len(s.Days) == 0 {
		return false
	}
	now = now.In(s.loc())
	if !s.Days[now.Weekday()] {
		return false
	}
	return now.Hour() == s.Hour && now.Minute() == s.Min
}

// InBlackout reports whether now falls inside the blackout window. The window
// may wrap past midnight (e.g. 22:00–06:00). During blackout, no sync runs —
// not the scheduler, and not a manual trigger.
func (s Schedule) InBlackout(now time.Time) bool {
	if !s.BlackoutEnabled || s.BlackoutFrom == s.BlackoutTo {
		return false
	}
	now = now.In(s.loc())
	t := now.Hour()*60 + now.Minute()
	if s.BlackoutFrom < s.BlackoutTo {
		return t >= s.BlackoutFrom && t < s.BlackoutTo
	}
	// Wraps midnight.
	return t >= s.BlackoutFrom || t < s.BlackoutTo
}

// loc returns the schedule's timezone, defaulting to UTC when unset (e.g. a
// zero-value Schedule).
func (s Schedule) loc() *time.Location {
	if s.Loc == nil {
		return time.UTC
	}
	return s.Loc
}

// parseHHMM parses "HH:MM"; invalid input yields 0,0.
func parseHHMM(v string) (h, m int) {
	parts := strings.SplitN(strings.TrimSpace(v), ":", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	h, _ = strconv.Atoi(parts[0])
	m, _ = strconv.Atoi(parts[1])
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0
	}
	return h, m
}

package main

import (
	"testing"
	"time"
)

func TestInScheduleWindow(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 7, 8, h, 30, 0, 0, time.UTC) }

	cases := []struct {
		hour, start, end int
		want             bool
	}{
		{3, 2, 5, true},    // inside a normal window
		{1, 2, 5, false},   // before the window
		{5, 2, 5, false},   // end is exclusive
		{23, 22, 5, true},  // wraps midnight — late night
		{2, 22, 5, true},   // wraps midnight — early morning
		{10, 22, 5, false}, // wraps midnight — daytime is outside
		{12, 0, 0, true},   // start == end means full day
	}
	for _, c := range cases {
		if got := inScheduleWindow(at(c.hour), c.start, c.end); got != c.want {
			t.Errorf("inScheduleWindow(hour=%d, [%d,%d)) = %v, want %v", c.hour, c.start, c.end, got, c.want)
		}
	}
}

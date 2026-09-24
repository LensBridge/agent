package store

import (
	"time"

	"github.com/LensBridge/agent/internal/mbu"
)

// Day is the answer to "which payload should the board show right now".
type Day struct {
	// Today is the current date in the bundle's timezone.
	Today string
	// Serving is Today clamped to firstDay..lastDay.
	Serving string
	// DaysRemaining is lastDay - today, floored at 0.
	DaysRemaining int
	// StaleDays is max(0, today - lastDay).
	StaleDays int
}

// PickDay applies the section 7 rule to the bundle at instant now.
func (c *Content) PickDay(now time.Time) Day {
	ci := c.Manifest.Content
	first, _ := time.Parse(mbu.DateLayout, ci.FirstDay)
	last, _ := time.Parse(mbu.DateLayout, ci.LastDay)
	y, mo, d := now.In(c.loc).Date()
	today := time.Date(y, mo, d, 0, 0, 0, 0, time.UTC)

	serving := today
	switch {
	case today.Before(first):
		serving = first
	case today.After(last):
		serving = last
	}
	return Day{
		Today:         today.Format(mbu.DateLayout),
		Serving:       serving.Format(mbu.DateLayout),
		DaysRemaining: max(0, daysBetween(today, last)),
		StaleDays:     max(0, daysBetween(last, today)),
	}
}

// daysBetween counts civil days from a to b (both at 00:00 UTC).
func daysBetween(a, b time.Time) int {
	return int(b.Sub(a).Hours() / 24)
}

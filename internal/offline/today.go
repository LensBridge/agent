package offline

import "time"

// Day is the answer to "what should the board show right now".
type Day struct {
	// Today is the current date in the bundle's timezone.
	Today string
	// Serving is the payload date served: Today clamped to firstDay..lastDay.
	Serving string
	// DaysRemaining is lastDay - today, floored at 0 (0 on the last day and
	// once the bundle has run out).
	DaysRemaining int
	// StaleDays is max(0, today - lastDay).
	StaleDays int
}

// PickDay applies the "Picking today's payload" rule to m at instant now.
func PickDay(m *Manifest, now time.Time) Day {
	today := civil(now.In(m.loc))

	serving := today
	switch {
	case today.Before(m.first):
		serving = m.first
	case today.After(m.last):
		serving = m.last
	}

	remaining := daysBetween(today, m.last)
	return Day{
		Today:         today.Format(dateLayout),
		Serving:       serving.Format(dateLayout),
		DaysRemaining: max(0, remaining),
		StaleDays:     max(0, -remaining),
	}
}

// civil drops the clock from t, keeping its wall-clock date, as midnight UTC.
// Every date comparison in this package is done on these, where a day is
// always exactly 24h long.
func civil(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// daysBetween returns b - a in whole days for two civil dates.
func daysBetween(a, b time.Time) int {
	return int(b.Sub(a).Hours() / 24)
}

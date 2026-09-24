package main

import (
	"fmt"
	"time"
)

// clockTolerance is how far apart the board's clock and the laptop's may be
// and still count as in step. It matches the board, which ignores an
// uploader's time closer than this (docs/architecture.md, section 10):
// the clocks are compared to the second, over a link with some latency.
const clockTolerance = 5 * time.Second

// clockCheck is the outcome of comparing the two clocks.
type clockCheck struct {
	// Drift is board minus laptop: positive means the board is ahead.
	Drift time.Duration
	// NeedsSet is true when |Drift| exceeds clockTolerance.
	NeedsSet bool
}

// compareClocks compares the board's clock, read by a request sent at sentAt
// and answered at gotAt, with the laptop's. The laptop's time is taken as the
// midpoint of the request, so a slow link does not read as drift.
func compareClocks(sentAt, gotAt time.Time, boardEpoch int64) clockCheck {
	laptop := sentAt.Add(gotAt.Sub(sentAt) / 2)
	drift := time.Unix(boardEpoch, 0).Sub(laptop.Truncate(time.Second))
	return clockCheck{Drift: drift, NeedsSet: drift.Abs() > clockTolerance}
}

// describeDrift puts a drift in words: "3 s behind this laptop".
func describeDrift(d time.Duration) string {
	secs := int64(d.Round(time.Second) / time.Second)
	switch {
	case secs == 0:
		return "in step with this laptop"
	case secs < 0:
		return humanSeconds(-secs) + " behind this laptop"
	default:
		return humanSeconds(secs) + " ahead of this laptop"
	}
}

func humanSeconds(s int64) string {
	switch {
	case s < 120:
		return fmt.Sprintf("%d s", s)
	case s < 2*3600:
		return fmt.Sprintf("%d min", s/60)
	case s < 2*86400:
		return fmt.Sprintf("%d h", s/3600)
	default:
		return fmt.Sprintf("%d days", s/86400)
	}
}

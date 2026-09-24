package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// clockTolerance is how far the board's clock may be from the laptop's
// before mbpush corrects it (docs/offline.md, "mbpush"). `date +%s` has
// one-second resolution and the round trip adds a little more, so anything
// much tighter would "fix" clocks that are already right.
const clockTolerance = 5 * time.Second

// clockCheck is the outcome of comparing the two clocks.
type clockCheck struct {
	// Drift is board minus laptop: positive means the board is ahead.
	Drift time.Duration
	// NeedsSet is true when |Drift| exceeds clockTolerance.
	NeedsSet bool
}

// compareClocks decides whether the board's clock needs setting. sentAt and
// gotAt bracket the remote `date +%s` call; the laptop's time is taken as
// their midpoint, so a slow link does not read as drift.
func compareClocks(sentAt, gotAt time.Time, boardEpoch int64) clockCheck {
	laptop := sentAt.Add(gotAt.Sub(sentAt) / 2)
	drift := time.Unix(boardEpoch, 0).Sub(laptop.Truncate(time.Second))
	abs := drift
	if abs < 0 {
		abs = -abs
	}
	return clockCheck{Drift: drift, NeedsSet: abs > clockTolerance}
}

// parseEpoch reads the output of `date +%s`.
func parseEpoch(out string) (int64, error) {
	s := strings.TrimSpace(out)
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("the board answered %q instead of a time", s)
	}
	return n, nil
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

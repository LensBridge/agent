//go:build !linux

package clock

import (
	"errors"
	"time"
)

// setSystemClock is only implemented on Linux, the only system the agent
// runs on; this stub lets the package build and test elsewhere.
func setSystemClock(time.Time) error {
	return errors.New("setting the system clock is only supported on Linux")
}

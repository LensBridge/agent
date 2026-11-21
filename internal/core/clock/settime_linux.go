//go:build linux

package clock

import (
	"fmt"
	"syscall"
	"time"
)

// setSystemClock sets CLOCK_REALTIME. The daemon's unit grants CAP_SYS_TIME
// for exactly this.
func setSystemClock(t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	if err := syscall.Settimeofday(&tv); err != nil {
		return fmt.Errorf("set the system clock: %w (the agent needs CAP_SYS_TIME)", err)
	}
	return nil
}

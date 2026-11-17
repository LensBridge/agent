// Package timesync says whether the system clock is synchronised to network
// time. It is its own package so that both the clock keeper and the importer
// (which the keeper imports) can ask.
package timesync

import (
	"os"
	"time"
)

// SyncedFile is created by systemd-timesyncd (systemd 239 and later) once it
// has synchronised the clock. It lives in /run, so it says "since this boot".
const SyncedFile = "/run/systemd/timesync/synchronized"

// Synced reports whether systemd-timesyncd has synchronised the clock since
// boot.
func Synced() bool {
	_, err := os.Stat(SyncedFile)
	return err == nil
}

// Now is the system clock and whether it is NTP-synchronised: the only time
// the board treats as an authority over signed or remembered times.
func Now() (time.Time, bool) {
	return time.Now(), Synced()
}

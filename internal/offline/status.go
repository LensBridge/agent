package offline

import (
	"errors"
	"time"
)

// Status is the /api/local/status object from Contract 2. The CLI's
// `status --json` prints the same object with clock and rtc added.
type Status struct {
	Mode     string      `json:"mode"`
	DeviceID string      `json:"deviceId"`
	Bundle   *BundleInfo `json:"bundle"`
	// Today is in the bundle's timezone, or the system's when no bundle is
	// installed (there is no other zone to use).
	Today string `json:"today"`
	// The rest describe the installed bundle and are null without one.
	ServingDay    *string `json:"servingDay"`
	DaysRemaining *int    `json:"daysRemaining"`
	StaleDays     *int    `json:"staleDays"`
	// Error is set when a bundle is installed but could not be read. Not in
	// the contract; additive, and absent in the normal case.
	Error string `json:"error,omitempty"`
}

// BundleInfo summarises the installed bundle's manifest.
type BundleInfo struct {
	FirstDay    string `json:"firstDay"`
	LastDay     string `json:"lastDay"`
	GeneratedAt string `json:"generatedAt"`
	Timezone    string `json:"timezone"`
}

// StatusFor builds the status for manifest m (nil when nothing is installed)
// at instant now.
func StatusFor(mode, deviceID string, m *Manifest, now time.Time) Status {
	s := Status{Mode: mode, DeviceID: deviceID}
	if m == nil {
		s.Today = now.Format(dateLayout)
		return s
	}
	d := PickDay(m, now)
	s.Bundle = &BundleInfo{
		FirstDay:    m.FirstDay,
		LastDay:     m.LastDay,
		GeneratedAt: m.GeneratedAt,
		Timezone:    m.Timezone,
	}
	s.Today = d.Today
	s.ServingDay = &d.Serving
	s.DaysRemaining = &d.DaysRemaining
	s.StaleDays = &d.StaleDays
	return s
}

// ReadStatus reads the current bundle from p and builds its status. Failure
// to read an installed bundle is reported in Status.Error, not returned, so
// callers always have something to show.
func ReadStatus(p Paths, mode, deviceID string, now time.Time) Status {
	_, m, err := CurrentBundle(p)
	if err != nil && !errors.Is(err, ErrNoBundle) {
		s := StatusFor(mode, deviceID, nil, now)
		s.Error = err.Error()
		return s
	}
	return StatusFor(mode, deviceID, m, now)
}

package localserver

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/LensBridge/agent/internal/clock"
	"github.com/LensBridge/agent/internal/importer"
	"github.com/LensBridge/agent/internal/selfupdate"
	"github.com/LensBridge/agent/internal/store"
	"github.com/LensBridge/agent/internal/updates"
)

// Status is /api/local/status (docs/architecture.md, section 7). The CLI and
// the upload server build theirs from it.
type Status struct {
	LocalAPI      int          `json:"localApi"`
	AgentVersion  string       `json:"agentVersion"`
	DeviceID      string       `json:"deviceId"`
	App           *AppInfo     `json:"app"`
	Content       *ContentInfo `json:"content"`
	Today         string       `json:"today"`
	ServingDay    *string      `json:"servingDay"`
	DaysRemaining *int         `json:"daysRemaining"`
	StaleDays     *int         `json:"staleDays"`
	Sync          any          `json:"sync"`
	Update        UpdateInfo   `json:"update"`
	// Clock says whether the board's clock can be believed; only the
	// daemon knows.
	Clock *clock.Info `json:"clock,omitempty"`
	// LastAgentUpdate is the root updater's last outcome, if any.
	LastAgentUpdate *selfupdate.Outcome `json:"lastAgentUpdate"`
	// Updates is the software waiting for its install window; only the
	// daemon knows it.
	Updates *updates.Info `json:"updates,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// AppInfo describes the installed app.
type AppInfo struct {
	Version     string `json:"version"`
	Source      string `json:"source,omitempty"`
	InstalledAt string `json:"installedAt,omitempty"`
}

// ContentInfo describes the installed content bundle.
type ContentInfo struct {
	Sequence    int64  `json:"sequence"`
	CreatedAt   string `json:"createdAt"`
	FirstDay    string `json:"firstDay"`
	LastDay     string `json:"lastDay"`
	Timezone    string `json:"timezone"`
	Source      string `json:"source,omitempty"`
	InstalledAt string `json:"installedAt,omitempty"`
}

// UpdateInfo says whether the update screen is up.
type UpdateInfo struct {
	Active bool `json:"active"`
}

// BuildStatus reads the installed state. It never fails: a board must always
// be able to say what it has, including that something is broken.
func BuildStatus(l store.Layout, deviceID, agentVersion string, now time.Time) Status {
	s := Status{LocalAPI: importer.LocalAPIVersion, AgentVersion: agentVersion, DeviceID: deviceID,
		LastAgentUpdate: selfupdate.ReadOutcome(l)}
	if a, err := l.CurrentApp(); err == nil {
		s.App = &AppInfo{Version: a.Manifest.Version}
	}
	c, err := l.CurrentContent()
	switch {
	case errors.Is(err, store.ErrNoContent):
		s.Today = now.Format("2006-01-02")
	case err != nil:
		s.Today = now.Format("2006-01-02")
		s.Error = err.Error()
	default:
		m := c.Manifest
		s.Content = &ContentInfo{
			Sequence: m.Sequence, CreatedAt: m.CreatedAt,
			FirstDay: m.Content.FirstDay, LastDay: m.Content.LastDay, Timezone: m.Content.Timezone,
			Source: c.Info.Source, InstalledAt: c.Info.InstalledAt,
		}
		d := c.PickDay(now)
		s.Today = d.Today
		s.ServingDay, s.DaysRemaining, s.StaleDays = &d.Serving, &d.DaysRemaining, &d.StaleDays
	}
	return s
}

// BoardReport is what the board tells LensBridge about itself with every
// heartbeat, for the admin portal: what it runs and shows, what is waiting to
// install, how its last agent update went, and whether its clock is right.
type BoardReport struct {
	AppVersion      string              `json:"appVersion,omitempty"`
	Content         *BoardContent       `json:"content,omitempty"`
	Updates         *updates.Info       `json:"updates,omitempty"`
	LastAgentUpdate *selfupdate.Outcome `json:"lastAgentUpdate,omitempty"`
	Clock           *clock.Info         `json:"clock,omitempty"`
	// SyncError is why the last content sync failed, if it did.
	SyncError string `json:"syncError,omitempty"`
	// Error is set when the installed content cannot be read.
	Error string `json:"error,omitempty"`
}

// BoardContent is the installed content, as the portal shows it.
type BoardContent struct {
	FirstDay      string `json:"firstDay"`
	LastDay       string `json:"lastDay"`
	CreatedAt     string `json:"createdAt"`
	Source        string `json:"source,omitempty"`
	InstalledAt   string `json:"installedAt,omitempty"`
	DaysRemaining int    `json:"daysRemaining"`
	StaleDays     int    `json:"staleDays"`
}

// Board is the heartbeat's report, taken from the status.
func (s Status) Board() BoardReport {
	r := BoardReport{Updates: s.Updates, LastAgentUpdate: s.LastAgentUpdate, Clock: s.Clock, Error: s.Error}
	if s.App != nil {
		r.AppVersion = s.App.Version
	}
	if c := s.Content; c != nil {
		r.Content = &BoardContent{FirstDay: c.FirstDay, LastDay: c.LastDay, CreatedAt: c.CreatedAt,
			Source: c.Source, InstalledAt: c.InstalledAt}
		if s.DaysRemaining != nil {
			r.Content.DaysRemaining = *s.DaysRemaining
		}
		if s.StaleDays != nil {
			r.Content.StaleDays = *s.StaleDays
		}
	}
	// Sync is the syncer's status object, whatever its type; only its
	// lastError is reported.
	if raw, err := json.Marshal(s.Sync); err == nil {
		var sync struct {
			LastError *string `json:"lastError"`
		}
		if json.Unmarshal(raw, &sync) == nil && sync.LastError != nil {
			r.SyncError = *sync.LastError
		}
	}
	return r
}

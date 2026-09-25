package localserver

import (
	"errors"
	"time"

	"github.com/LensBridge/agent/internal/importer"
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
	s := Status{LocalAPI: importer.LocalAPIVersion, AgentVersion: agentVersion, DeviceID: deviceID}
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

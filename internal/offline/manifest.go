package offline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// FormatVersion is the only bundle format this agent understands.
const FormatVersion = 1

// maxWindowDays bounds firstDay..lastDay. The backend allows 1–31; a larger
// range is not a bundle we were designed to receive.
const maxWindowDays = 31

const dateLayout = "2006-01-02"

// Entry-name rules from Contract 1. These are the whole zip-slip defence:
// nothing that fails them is ever joined onto a filesystem path.
var (
	payloadNameRE = regexp.MustCompile(`^payloads/(\d{4}-\d{2}-\d{2})\.json$`)
	mediaNameRE   = regexp.MustCompile(`^media/([0-9a-f]{64})\.([A-Za-z0-9]{1,10})$`)
	// mediaFileRE is a media file's base name, as requested under /media/.
	mediaFileRE = regexp.MustCompile(`^[0-9a-f]{64}\.[A-Za-z0-9]{1,10}$`)
)

// Manifest is manifest.json. See Contract 1.
type Manifest struct {
	FormatVersion int          `json:"formatVersion"`
	DeviceID      string       `json:"deviceId"`
	Timezone      string       `json:"timezone"`
	GeneratedAt   string       `json:"generatedAt"`
	FirstDay      string       `json:"firstDay"`
	LastDay       string       `json:"lastDay"`
	Media         []MediaEntry `json:"media"`

	// Parsed forms, filled by parseManifest.
	loc         *time.Location
	generatedAt time.Time
	first, last time.Time // civil dates at 00:00 UTC
}

// MediaEntry describes one file under media/.
type MediaEntry struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
	ContentType string `json:"contentType"`
}

// Location is the manifest's timezone. "Today" is always evaluated in it.
func (m *Manifest) Location() *time.Location { return m.loc }

// Days lists every date firstDay..lastDay inclusive.
func (m *Manifest) Days() []string {
	var out []string
	for d := m.first; !d.After(m.last); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(dateLayout))
	}
	return out
}

// parseManifest decodes raw and checks everything about it that does not
// depend on the rest of the zip or on which device is installing it.
func parseManifest(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("manifest.json is not valid JSON: %w", err)
	}
	if m.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("manifest.json has formatVersion %d; this agent only understands %d (update the agent, or export the bundle again)",
			m.FormatVersion, FormatVersion)
	}
	if m.DeviceID == "" {
		return nil, fmt.Errorf("manifest.json has no deviceId")
	}

	// LoadLocation("") and ("Local") both succeed, and neither is a zone.
	if m.Timezone == "" || m.Timezone == "Local" {
		return nil, fmt.Errorf("manifest.json has no usable timezone (%q)", m.Timezone)
	}
	loc, err := time.LoadLocation(m.Timezone)
	if err != nil {
		return nil, fmt.Errorf("manifest.json timezone %q is not known on this board: %w", m.Timezone, err)
	}
	m.loc = loc

	if m.generatedAt, err = time.Parse(time.RFC3339, m.GeneratedAt); err != nil {
		return nil, fmt.Errorf("manifest.json generatedAt %q is not an ISO-8601 timestamp", m.GeneratedAt)
	}
	if m.first, err = parseDate(m.FirstDay); err != nil {
		return nil, fmt.Errorf("manifest.json firstDay: %w", err)
	}
	if m.last, err = parseDate(m.LastDay); err != nil {
		return nil, fmt.Errorf("manifest.json lastDay: %w", err)
	}
	if m.last.Before(m.first) {
		return nil, fmt.Errorf("manifest.json lastDay %s is before firstDay %s", m.LastDay, m.FirstDay)
	}
	if n := daysBetween(m.first, m.last) + 1; n > maxWindowDays {
		return nil, fmt.Errorf("manifest.json covers %d days; at most %d are allowed", n, maxWindowDays)
	}

	seen := make(map[string]bool, len(m.Media))
	for _, e := range m.Media {
		sub := mediaNameRE.FindStringSubmatch(e.Path)
		if sub == nil {
			return nil, fmt.Errorf("manifest.json media path %q is not media/<sha256>.<ext>", e.Path)
		}
		if sub[1] != e.SHA256 {
			return nil, fmt.Errorf("manifest.json media %s: sha256 %q does not match its file name", e.Path, e.SHA256)
		}
		if e.Bytes < 0 {
			return nil, fmt.Errorf("manifest.json media %s: negative size", e.Path)
		}
		if seen[e.Path] {
			return nil, fmt.Errorf("manifest.json lists %s twice", e.Path)
		}
		seen[e.Path] = true
	}
	return &m, nil
}

// readManifest loads and parses the manifest of an installed bundle.
func readManifest(bundleDir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(bundleDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	return parseManifest(raw)
}

// mediaByFile indexes the manifest's media by base name ("<sha>.<ext>").
func (m *Manifest) mediaByFile() map[string]MediaEntry {
	out := make(map[string]MediaEntry, len(m.Media))
	for _, e := range m.Media {
		out[strings.TrimPrefix(e.Path, "media/")] = e
	}
	return out
}

func parseDate(s string) (time.Time, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a YYYY-MM-DD date", s)
	}
	return t, nil
}

// compactStamp turns generatedAt into a directory-name-safe stamp:
// 2026-09-24T14:02:11Z -> 20260924T140211Z.
func (m *Manifest) compactStamp() string {
	return m.generatedAt.UTC().Format("20060102T150405Z")
}

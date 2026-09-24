// Package mbu reads, verifies and writes MusallahBoard update packages
// (".mbu", docs/architecture.md section 4).
//
// A package is a zip holding mbu.json (the manifest), mbu.sig (Ed25519
// signatures over the manifest) and the files the manifest lists with their
// SHA-256 and size. Every kind of change to a board (content, the board app,
// the agent) travels in this one format, so there is exactly one verifier and
// every transport is an untrusted pipe into it.
//
// Nothing in a package is used as a filesystem path, or trusted at all, until
// the signature has verified and the entry has matched the manifest.
package mbu

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// Format and FormatVersion identify this layout. Version 1 was the
	// unsigned v1 offline bundle; it is not accepted.
	Format        = "mbu"
	FormatVersion = 2

	// SignaturePrefix is prepended to the manifest bytes before signing:
	// domain separation from the agent's other two Ed25519 messages
	// (musallahboard-auth-v1, musallahboard-http-v1).
	SignaturePrefix = "musallahboard-mbu-v2\n"

	ManifestName  = "mbu.json"
	SignatureName = "mbu.sig"

	// DateLayout is a content package's day format.
	DateLayout = "2006-01-02"
)

// Limits from section 4. They bound what a small hostile file can make the
// board read, allocate or write before its hashes are even checked.
const (
	MaxPackageBytes   = 512 << 20
	MaxManifestBytes  = 1 << 20
	MaxSignatureBytes = 64 << 10
	MaxFiles          = 20000
	MaxTotalBytes     = 1 << 30
	MaxPayloadBytes   = 16 << 20
	MaxMediaBytes     = 512 << 20
	MaxContentDays    = 31
	maxPathBytes      = 200
	maxPathSegments   = 8
)

// Type is what a package installs.
type Type string

const (
	TypeContent Type = "content"
	TypeApp     Type = "app"
	TypeAgent   Type = "agent"
)

// File is one entry of the manifest's files list.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Manifest is mbu.json. Pointer fields distinguish "absent" from "zero" so a
// missing required field is caught rather than read as 0 or "".
type Manifest struct {
	Format        string       `json:"format"`
	FormatVersion int          `json:"formatVersion"`
	Type          Type         `json:"type"`
	CreatedAt     string       `json:"createdAt"`
	Sequence      int64        `json:"sequence,omitempty"`
	DeviceID      string       `json:"deviceId,omitempty"`
	Version       string       `json:"version,omitempty"`
	Files         []File       `json:"files"`
	Content       *ContentInfo `json:"content,omitempty"`
	App           *AppInfo     `json:"app,omitempty"`
	Agent         *AgentInfo   `json:"agent,omitempty"`

	createdAt time.Time
	byPath    map[string]File
}

// ContentInfo is the content-specific part of a content manifest.
type ContentInfo struct {
	Timezone string      `json:"timezone"`
	FirstDay string      `json:"firstDay"`
	LastDay  string      `json:"lastDay"`
	Media    []MediaInfo `json:"media"`
}

// MediaInfo gives a media file its content type.
type MediaInfo struct {
	Path        string `json:"path"`
	ContentType string `json:"contentType"`
}

// AppInfo is the app-specific part of an app manifest.
type AppInfo struct {
	// LocalAPI is the local kiosk API version the build needs.
	LocalAPI int `json:"localApi"`
}

// AgentInfo is the agent-specific part of an agent manifest.
type AgentInfo struct {
	Arch   string `json:"arch"`
	Binary string `json:"binary"`
}

// CreatedTime is createdAt, parsed.
func (m *Manifest) CreatedTime() time.Time { return m.createdAt }

// File returns the listed file at path.
func (m *Manifest) File(path string) (File, bool) {
	f, ok := m.byPath[path]
	return f, ok
}

// Describe is a short human name for the package, for logs and screens.
func (m *Manifest) Describe() string {
	switch m.Type {
	case TypeContent:
		if m.Content != nil {
			return fmt.Sprintf("content %s to %s", m.Content.FirstDay, m.Content.LastDay)
		}
		return "content"
	case TypeApp:
		return "board app " + m.Version
	case TypeAgent:
		return "agent " + m.Version
	}
	return string(m.Type)
}

var (
	segmentRE  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	sha256RE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	versionRE  = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)
	payloadRE  = regexp.MustCompile(`^payloads/(\d{4}-\d{2}-\d{2})\.json$`)
	mediaRE    = regexp.MustCompile(`^media/([0-9a-f]{64})\.([a-z0-9]{1,10})$`)
	uuidLikeRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// MediaFileRE matches a media file's base name as requested under /media/.
var MediaFileRE = regexp.MustCompile(`^[0-9a-f]{64}\.[a-z0-9]{1,10}$`)

// allowedMediaTypes are the content types a content package may declare.
var allowedMediaTypes = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true,
	"image/avif": true, "image/svg+xml": true, "application/octet-stream": true,
}

// ValidPath reports whether name is an acceptable entry or file path. This is
// the whole zip-slip defence: a path that passes can be joined onto a
// directory without escaping it.
func ValidPath(name string) error {
	if name == "" || len(name) > maxPathBytes {
		return fmt.Errorf("path %q is empty or longer than %d bytes", name, maxPathBytes)
	}
	segs := strings.Split(name, "/")
	if len(segs) > maxPathSegments {
		return fmt.Errorf("path %q is nested more than %d deep", name, maxPathSegments)
	}
	for _, s := range segs {
		if !segmentRE.MatchString(s) {
			return fmt.Errorf("path %q has a disallowed segment %q", name, s)
		}
	}
	return nil
}

// ValidVersion reports whether v is MAJOR.MINOR.PATCH.
func ValidVersion(v string) bool { return versionRE.MatchString(v) }

// CompareVersions compares two MAJOR.MINOR.PATCH versions: -1, 0 or 1. An
// invalid version sorts before every valid one, so "dev" is older than any
// release.
func CompareVersions(a, b string) int {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := range 3 {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersion(v string) ([3]int64, bool) {
	var out [3]int64
	if !ValidVersion(v) {
		return out, false
	}
	for i, p := range strings.SplitN(v, ".", 3) {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ParseManifest decodes raw and checks everything about it that does not
// depend on the rest of the zip, on keys or on which board reads it.
func ParseManifest(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("mbu.json is not valid JSON: %w", err)
	}
	if m.Format != Format {
		return nil, fmt.Errorf("mbu.json format is %q, want %q (is this a MusallahBoard update package?)", m.Format, Format)
	}
	if m.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("package format version %d is not supported by this agent (it reads %d); update the agent or rebuild the package",
			m.FormatVersion, FormatVersion)
	}
	t, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("mbu.json createdAt %q is not an RFC 3339 time", m.CreatedAt)
	}
	m.createdAt = t

	if len(m.Files) > MaxFiles {
		return nil, fmt.Errorf("package lists %d files; at most %d are allowed", len(m.Files), MaxFiles)
	}
	m.byPath = make(map[string]File, len(m.Files))
	var total int64
	for _, f := range m.Files {
		if err := ValidPath(f.Path); err != nil {
			return nil, fmt.Errorf("mbu.json: %w", err)
		}
		if f.Path == ManifestName || f.Path == SignatureName {
			return nil, fmt.Errorf("mbu.json lists %s as a file", f.Path)
		}
		if !sha256RE.MatchString(f.SHA256) {
			return nil, fmt.Errorf("mbu.json: %s has a malformed sha256", f.Path)
		}
		if f.Bytes < 0 {
			return nil, fmt.Errorf("mbu.json: %s has a negative size", f.Path)
		}
		if _, dup := m.byPath[f.Path]; dup {
			return nil, fmt.Errorf("mbu.json lists %s twice", f.Path)
		}
		m.byPath[f.Path] = f
		total += f.Bytes
		if total > MaxTotalBytes {
			return nil, fmt.Errorf("package files total more than %d MiB", MaxTotalBytes>>20)
		}
	}

	switch m.Type {
	case TypeContent:
		err = m.checkContent()
	case TypeApp:
		err = m.checkApp()
	case TypeAgent:
		err = m.checkAgent()
	default:
		err = fmt.Errorf("mbu.json type %q is not content, app or agent", m.Type)
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) checkContent() error {
	if !uuidLikeRE.MatchString(m.DeviceID) {
		return fmt.Errorf("content package has no valid deviceId")
	}
	if m.Sequence <= 0 {
		return fmt.Errorf("content package has no positive sequence")
	}
	c := m.Content
	if c == nil {
		return fmt.Errorf("content package has no content section")
	}
	if c.Timezone == "" || c.Timezone == "Local" {
		return fmt.Errorf("content package has no usable timezone (%q)", c.Timezone)
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("content timezone %q is not known on this board: %w", c.Timezone, err)
	}
	days, err := c.Days()
	if err != nil {
		return err
	}

	want := make(map[string]bool, len(days))
	for _, d := range days {
		want[d] = true
		f, ok := m.byPath["payloads/"+d+".json"]
		if !ok {
			return fmt.Errorf("content package is missing payloads/%s.json (it covers %s to %s)", d, c.FirstDay, c.LastDay)
		}
		if f.Bytes > MaxPayloadBytes {
			return fmt.Errorf("payloads/%s.json is larger than %d MiB", d, MaxPayloadBytes>>20)
		}
	}

	types := make(map[string]string, len(c.Media))
	for _, mi := range c.Media {
		if !allowedMediaTypes[mi.ContentType] {
			return fmt.Errorf("media %s has content type %q, which is not allowed", mi.Path, mi.ContentType)
		}
		if _, dup := types[mi.Path]; dup {
			return fmt.Errorf("content media lists %s twice", mi.Path)
		}
		types[mi.Path] = mi.ContentType
	}

	var mediaTotal int64
	for _, f := range m.Files {
		switch {
		case payloadRE.MatchString(f.Path):
			if d := payloadRE.FindStringSubmatch(f.Path)[1]; !want[d] {
				return fmt.Errorf("content package has %s, outside its range %s to %s", f.Path, c.FirstDay, c.LastDay)
			}
		case mediaRE.MatchString(f.Path):
			if mediaRE.FindStringSubmatch(f.Path)[1] != f.SHA256 {
				return fmt.Errorf("media %s: its name does not match its sha256", f.Path)
			}
			if _, ok := types[f.Path]; !ok {
				return fmt.Errorf("media %s has no content type in the content section", f.Path)
			}
			mediaTotal += f.Bytes
		default:
			return fmt.Errorf("content package lists %q, which is neither payloads/<date>.json nor media/<sha256>.<ext>", f.Path)
		}
	}
	for p := range types {
		if _, ok := m.byPath[p]; !ok {
			return fmt.Errorf("content media lists %s, which is not in files", p)
		}
	}
	if mediaTotal > MaxMediaBytes {
		return fmt.Errorf("content media totals %d MiB; at most %d MiB is allowed", mediaTotal>>20, MaxMediaBytes>>20)
	}
	return nil
}

func (m *Manifest) checkApp() error {
	if !ValidVersion(m.Version) {
		return fmt.Errorf("app package version %q is not MAJOR.MINOR.PATCH", m.Version)
	}
	if m.App == nil || m.App.LocalAPI <= 0 {
		return fmt.Errorf("app package has no app.localApi")
	}
	if _, ok := m.byPath["index.html"]; !ok {
		return fmt.Errorf("app package has no index.html at its root")
	}
	return nil
}

func (m *Manifest) checkAgent() error {
	if !ValidVersion(m.Version) {
		return fmt.Errorf("agent package version %q is not MAJOR.MINOR.PATCH", m.Version)
	}
	a := m.Agent
	if a == nil || (a.Arch != "arm64" && a.Arch != "amd64") {
		return fmt.Errorf("agent package has no valid agent.arch")
	}
	if len(m.Files) != 1 || m.Files[0].Path != a.Binary {
		return fmt.Errorf("agent package must list exactly one file, its binary %q", a.Binary)
	}
	return nil
}

// Days lists every date firstDay..lastDay inclusive, checking the range.
func (c *ContentInfo) Days() ([]string, error) {
	first, err := time.Parse(DateLayout, c.FirstDay)
	if err != nil {
		return nil, fmt.Errorf("content firstDay %q is not a YYYY-MM-DD date", c.FirstDay)
	}
	last, err := time.Parse(DateLayout, c.LastDay)
	if err != nil {
		return nil, fmt.Errorf("content lastDay %q is not a YYYY-MM-DD date", c.LastDay)
	}
	if last.Before(first) {
		return nil, fmt.Errorf("content lastDay %s is before firstDay %s", c.LastDay, c.FirstDay)
	}
	if n := int(last.Sub(first).Hours()/24) + 1; n > MaxContentDays {
		return nil, fmt.Errorf("content covers %d days; at most %d are allowed", n, MaxContentDays)
	}
	var out []string
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(DateLayout))
	}
	return out, nil
}

// MediaType returns the declared content type of the media file with base
// name file ("<sha>.<ext>"), or "".
func (c *ContentInfo) MediaType(file string) string {
	for _, mi := range c.Media {
		if mi.Path == "media/"+file {
			return mi.ContentType
		}
	}
	return ""
}

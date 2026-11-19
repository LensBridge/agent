// Package config is the board's /etc/musallahboard/agent.toml
// (docs/architecture.md, section 11). Every key is optional: a board with no
// agent.toml at all is an unprovisioned board, which still runs, shows its
// "not set up yet" screen and takes USB sticks.
//
// Identity, written by a provision package (setup file) or `enroll`:
//
//	device_id          = "uuid"
//	display_name       = "North Musallah"
//	provision_sequence = 1700000000000
//
// The optional online layer, also from the setup file or `enroll`:
//
//	backend_url   = "https://api.example.org"
//	claim_secret  = "<base64url>"   until the board has claimed its device key
//	websocket_url = "wss://api.example.org/api/agent/ws"   from the claim or enroll reply
//
// backend_url must be https:// and websocket_url wss:// on the same host,
// unless insecure_dev = true (CheckTransport). The device key is always at
// KeyPath.
//
// Board options: service_port, usb_import, content_sync, content_days,
// auto_update, app_channel_url, agent_channel_url, update_time, insecure_dev.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/LensBridge/agent/internal/core/fsutil"
)

// DefaultPath is agent.toml on a board.
const DefaultPath = "/etc/musallahboard/agent.toml"

// KeyPath is the device key (package keystore), next to agent.toml.
const KeyPath = "/etc/musallahboard/agent.key"

// Channel defaults (section 9.4). %s is the board's GOARCH.
const (
	DefaultAppChannelURL   = "https://github.com/LensBridge/MusallahBoard/releases/latest/download/app-channel.json"
	DefaultAgentChannelURL = "https://github.com/LensBridge/agent/releases/latest/download/agent-channel-%s.json"
	DefaultContentDays     = 7
	DefaultUpdateHour      = 23
)

// Config is agent.toml. Every field is optional. For the options, a nil
// pointer means "not set" and the accessor methods supply the default.
type Config struct {
	DeviceID          string `toml:"device_id,omitempty"`
	DisplayName       string `toml:"display_name,omitempty"`
	ProvisionSequence int64  `toml:"provision_sequence,omitempty"`
	BackendURL        string `toml:"backend_url,omitempty"`
	ClaimSecret       string `toml:"claim_secret,omitempty"`
	WebSocketURL      string `toml:"websocket_url,omitempty"`

	ServicePortSet  *bool   `toml:"service_port,omitempty"`
	USBImportSet    *bool   `toml:"usb_import,omitempty"`
	ContentSyncSet  *bool   `toml:"content_sync,omitempty"`
	ContentDaysSet  *int    `toml:"content_days,omitempty"`
	AutoUpdateSet   *bool   `toml:"auto_update,omitempty"`
	AppChannelSet   *string `toml:"app_channel_url,omitempty"`
	AgentChannelSet *string `toml:"agent_channel_url,omitempty"`
	UpdateTimeSet   *string `toml:"update_time,omitempty"`

	// InsecureDevSet is the explicit development opt-in to plaintext
	// http:// and ws:// backends (enroll --insecure-dev). Written to
	// agent.toml so that what the daemon accepts is visible there.
	InsecureDevSet *bool `toml:"insecure_dev,omitempty"`
}

// Provisioned reports whether the board has an identity (a device id).
func (c *Config) Provisioned() bool { return c.DeviceID != "" }

// Online reports whether the board is set up for the online layer: it has
// an identity and a backend to talk to. It may still have to claim its
// device key first (no websocket_url yet).
func (c *Config) Online() bool { return c.DeviceID != "" && c.BackendURL != "" }

// InsecureDev reports whether this board may talk to its backend without TLS.
// Development boards only.
func (c *Config) InsecureDev() bool { return boolOr(c.InsecureDevSet, false) }

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// ServicePort reports whether the eth0 service port and upload server run.
func (c *Config) ServicePort() bool {
	return boolOr(c.ServicePortSet, false)
}

// USBImport reports whether USB sticks are accepted.
func (c *Config) USBImport() bool { return boolOr(c.USBImportSet, true) }

// ContentSync reports whether content is synced from the backend.
func (c *Config) ContentSync() bool { return boolOr(c.ContentSyncSet, true) }

// AutoUpdate reports whether the release channels are followed.
func (c *Config) AutoUpdate() bool { return boolOr(c.AutoUpdateSet, true) }

// ContentDays is the window of a synced content package, 1-31.
func (c *Config) ContentDays() int {
	if c.ContentDaysSet == nil || *c.ContentDaysSet < 1 || *c.ContentDaysSet > 31 {
		return DefaultContentDays
	}
	return *c.ContentDaysSet
}

// AppChannelURL is the app release channel, "" when disabled.
func (c *Config) AppChannelURL() string {
	if c.AppChannelSet != nil {
		return *c.AppChannelSet
	}
	return DefaultAppChannelURL
}

// AgentChannelURL is the agent release channel for arch, "" when disabled.
func (c *Config) AgentChannelURL(arch string) string {
	if c.AgentChannelSet != nil {
		return *c.AgentChannelSet
	}
	return fmt.Sprintf(DefaultAgentChannelURL, arch)
}

// UpdateTime is when downloaded software installs, as hour and minute in the
// board's time zone (update_time, "HH:MM", default 23:00). A malformed value
// falls back to the default rather than stopping updates.
func (c *Config) UpdateTime() (hour, minute int) {
	if c.UpdateTimeSet != nil {
		if t, err := time.Parse("15:04", strings.TrimSpace(*c.UpdateTimeSet)); err == nil {
			return t.Hour(), t.Minute()
		}
	}
	return DefaultUpdateHour, 0
}

// ErrInsecureTransport marks a backend or WebSocket URL that is refused
// because it is not TLS (or not where the backend is) and insecure_dev is off.
var ErrInsecureTransport = errors.New("insecure backend transport")

// CheckBackendURL checks a backend base URL: https:// with a host, no
// credentials, query or fragment. http:// is accepted only with insecureDev.
//
// Over plain http the enrollment reply, which pins the content signing keys
// and the WebSocket URL, can be rewritten by anyone on the LAN; so can every
// later sync. TLS is what makes the pinned keys mean anything.
func CheckBackendURL(raw string, insecureDev bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("backend URL %q: %w", raw, err)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && insecureDev:
	case u.Scheme == "http":
		return fmt.Errorf("%w: backend URL %s is plain http; use https:// (development boards only: enroll with --insecure-dev, which sets insecure_dev = true)", ErrInsecureTransport, raw)
	default:
		return fmt.Errorf("backend URL %q must start with https://", raw)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("backend URL %q has no host", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("backend URL %q must be a plain base URL (no credentials, query or fragment)", raw)
	}
	return nil
}

// CheckWebSocketURL checks the WebSocket URL the backend handed out at
// enrollment against the backend URL it was enrolled with: wss:// (ws:// only
// with insecureDev), and on the same host.
//
// Same host, not same origin: the port may differ. TLS binds the server's
// identity to the host name, not the port, and one host serving the API and
// the socket on different ports is the same operator. A different host is
// refused: the device signs auth challenges for whoever answers there and
// takes reboot, screenshot and log commands from it, so it has to be the
// backend the board enrolled with. There is no override for split-host
// deployments; put the socket behind the backend's host instead.
func CheckWebSocketURL(raw, backendURL string, insecureDev bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("websocket URL %q: %w", raw, err)
	}
	switch {
	case u.Scheme == "wss":
	case u.Scheme == "ws" && insecureDev:
	case u.Scheme == "ws":
		return fmt.Errorf("%w: websocket URL %s is plain ws; the backend must hand out wss:// (development boards only: insecure_dev = true)", ErrInsecureTransport, raw)
	default:
		return fmt.Errorf("websocket URL %q must start with wss://", raw)
	}
	be, err := url.Parse(strings.TrimSpace(backendURL))
	if err != nil {
		return fmt.Errorf("backend URL %q: %w", backendURL, err)
	}
	if u.Hostname() == "" || !sameHost(u.Hostname(), be.Hostname()) {
		return fmt.Errorf("%w: websocket URL %s is not on the backend's host %s", ErrInsecureTransport, raw, be.Hostname())
	}
	if u.User != nil {
		return fmt.Errorf("websocket URL %q must not carry credentials", raw)
	}
	return nil
}

func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

// CheckTransport applies CheckBackendURL and CheckWebSocketURL to this
// config. The daemon calls it before it builds anything that talks to the
// backend (content sync, release checks, the command
// WebSocket) and talks to none of them when it fails.
//
// Load does not call it: a board with an insecure config still shows the
// content it has (local first), and the local CLI tools still work on it.
func (c *Config) CheckTransport() error {
	if err := CheckBackendURL(c.BackendURL, c.InsecureDev()); err != nil {
		return err
	}
	return CheckWebSocketURL(c.WebSocketURL, c.BackendURL, c.InsecureDev())
}

// Load reads the config at path. A missing file is the empty config of an
// unprovisioned board, not an error.
func Load(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return &c, nil
}

// SetServicePort rewrites service_port in the config at path (creating it if
// need be), leaving every other field as it was.
func SetServicePort(path string, on bool) error {
	c, err := Load(path)
	if err != nil {
		return err
	}
	c.ServicePortSet = &on
	return Save(path, c)
}

// Save writes the config atomically: tmp file + fsync + rename. Survives a
// power cut mid-write — either the old file is intact or the new one is.
//
// The file gets the owner of its directory. Root tools (`enroll`,
// `service-port`) write it under sudo, but the daemon that reads it runs as
// the service user, which owns /etc/musallahboard; run as that user, this is
// a no-op.
func Save(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("encode: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close: %w", err)
	}
	fsutil.ChownToDirOwner(tmp, filepath.Dir(path))
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// Live is agent.toml as the daemon holds it: read once at start, changed by
// the importer when it applies a setup file (and by the online layer when a
// claim completes), and read again by everything that needs the board's
// identity. Each change is saved before anyone sees it.
type Live struct {
	path string

	mu      sync.Mutex
	cur     Config
	changed chan struct{}
}

// Open reads the config at path into a Live. On an error it still returns a
// Live, holding the empty config, so the board runs as unprovisioned while
// the caller reports why.
func Open(path string) (*Live, error) {
	l := &Live{path: path, changed: make(chan struct{})}
	c, err := Load(path)
	if err != nil {
		return l, err
	}
	l.cur = *c
	return l, nil
}

// InMemory is a Live that holds c and never saves it, for tests and tools.
func InMemory(c Config) *Live {
	return &Live{cur: c, changed: make(chan struct{})}
}

// Get returns a copy of the current config.
func (l *Live) Get() *Config {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.cur
	return &c
}

// Update applies fn to a copy of the current config and saves it; only then
// does it become current. fn's error aborts without saving.
func (l *Live) Update(fn func(*Config) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	next := l.cur
	if err := fn(&next); err != nil {
		return err
	}
	if l.path != "" {
		if err := Save(l.path, &next); err != nil {
			return err
		}
	}
	l.cur = next
	close(l.changed)
	l.changed = make(chan struct{})
	return nil
}

// Changed returns a channel closed at the next Update.
func (l *Live) Changed() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.changed
}

// Package config persists the agent's runtime identity at /etc/musallahboard/agent.toml.
//
// Schema:
//
//	device_id      = "uuid"
//	backend_url    = "https://api.utmmsa.ca"
//	websocket_url  = "wss://api.utmmsa.ca/api/agent/ws"
//	key_path       = "/etc/musallahboard/agent.key"
//
// Optional keys (docs/architecture.md, section 11): service_port, usb_import,
// content_sync, content_days, auto_update, app_channel_url, agent_channel_url.
//
// Identity is proved by signing a server-issued challenge with the Ed25519 key
// at KeyPath.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// DefaultKeyPath is where Save writes if the caller leaves Config.KeyPath empty.
const DefaultKeyPath = "/etc/musallahboard/agent.key"

// Channel defaults (section 9.4). %s is the board's GOARCH.
const (
	DefaultAppChannelURL   = "https://github.com/LensBridge/MusallahBoard/releases/latest/download/app-channel.json"
	DefaultAgentChannelURL = "https://github.com/LensBridge/agent/releases/latest/download/agent-channel-%s.json"
	DefaultContentDays     = 7
)

// Config is the persisted agent configuration written at enrollment.
//
// The first four fields are required at runtime; Load returns an error if any
// is empty. Everything else is optional: a nil pointer means "not set", and
// the accessor methods supply the default, so a freshly enrolled config needs
// none of them.
type Config struct {
	DeviceID     string `toml:"device_id"`
	BackendURL   string `toml:"backend_url"`
	WebSocketURL string `toml:"websocket_url"`
	KeyPath      string `toml:"key_path"`

	ServicePortSet  *bool   `toml:"service_port,omitempty"`
	USBImportSet    *bool   `toml:"usb_import,omitempty"`
	ContentSyncSet  *bool   `toml:"content_sync,omitempty"`
	ContentDaysSet  *int    `toml:"content_days,omitempty"`
	AutoUpdateSet   *bool   `toml:"auto_update,omitempty"`
	AppChannelSet   *string `toml:"app_channel_url,omitempty"`
	AgentChannelSet *string `toml:"agent_channel_url,omitempty"`
}

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

func Load(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if c.KeyPath == "" {
		c.KeyPath = DefaultKeyPath
	}
	if c.DeviceID == "" || c.BackendURL == "" || c.WebSocketURL == "" {
		return nil, fmt.Errorf("config %s is incomplete (run `agent enroll` first)", path)
	}
	return &c, nil
}

// SetServicePort rewrites service_port in the config at path, leaving every
// other field as it was. The file
// keeps its owner: this runs as root via sudo, but the daemon reading the
// result runs as the service user, and Save creates a fresh 0600 file that
// would otherwise belong to root.
func SetServicePort(path string, on bool) error {
	c, err := Load(path)
	if err != nil {
		return err
	}
	owner, haveOwner := fileOwner(path)
	c.ServicePortSet = &on
	if err := Save(path, c); err != nil {
		return err
	}
	if haveOwner {
		owner.apply(path)
	}
	return nil
}

// Save writes the config atomically: tmp file + fsync + rename. Survives a
// power cut mid-write — either the old file is intact or the new one is.
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
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

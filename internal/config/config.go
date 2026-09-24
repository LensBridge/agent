// Package config persists the agent's runtime identity at /etc/musallahboard/agent.toml.
//
// Schema (v2 — Ed25519 era):
//
//	device_id      = "uuid"
//	backend_url    = "https://api.utmmsa.ca"
//	websocket_url  = "wss://api.utmmsa.ca/api/agent/ws"
//	key_path       = "/etc/musallahboard/agent.key"
//	mode           = "offline"   # optional; absent means "online"
//
// The HMAC `device_secret` field from the v1 schema is gone; identity is now
// proved by signing a server-issued challenge with the Ed25519 key at KeyPath.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// DefaultKeyPath is where Save writes if the caller leaves Config.KeyPath empty.
const DefaultKeyPath = "/etc/musallahboard/agent.key"

// Operating modes. Online is the original behaviour: WebSocket to the backend,
// telemetry, remote commands. Offline makes no network calls and serves an
// installed content bundle to the kiosk from 127.0.0.1 (see docs/offline.md).
const (
	ModeOnline  = "online"
	ModeOffline = "offline"
)

// Config is the persisted agent configuration written at enrollment.
//
// The first four fields are required at runtime; Load returns an error if any
// is empty. Mode is optional and Load normalizes an absent one to ModeOnline, so
// every config written before offline mode existed keeps behaving as it did.
type Config struct {
	DeviceID     string `toml:"device_id"`
	BackendURL   string `toml:"backend_url"`
	WebSocketURL string `toml:"websocket_url"`
	KeyPath      string `toml:"key_path"`
	Mode         string `toml:"mode,omitempty"`
}

// ValidateMode reports whether m is a mode the agent can run in. The empty
// string is not accepted here; Load maps it to ModeOnline before validating.
func ValidateMode(m string) error {
	switch m {
	case ModeOnline, ModeOffline:
		return nil
	}
	return fmt.Errorf("unknown mode %q (want %q or %q)", m, ModeOnline, ModeOffline)
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
	if c.Mode == "" {
		c.Mode = ModeOnline
	}
	if err := ValidateMode(c.Mode); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

// SetMode rewrites the mode key of the config at path, leaving every other
// field as it was. The file keeps its owner: `mode` runs as root via sudo, but
// the daemon reading the result runs as the service user, and Save creates a
// fresh 0600 file that would otherwise belong to root.
func SetMode(path, mode string) error {
	if err := ValidateMode(mode); err != nil {
		return err
	}
	c, err := Load(path)
	if err != nil {
		return err
	}
	owner, haveOwner := fileOwner(path)
	c.Mode = mode
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

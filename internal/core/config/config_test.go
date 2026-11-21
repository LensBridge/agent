package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseTOML keeps key_path, which configs written before it was dropped
// carry: an unknown key must not stop a board from loading its config.
const baseTOML = `device_id = "3f2a1b4c-0000-4000-8000-000000000001"
backend_url = "https://api.example.com"
websocket_url = "wss://api.example.com/api/agent/ws"
key_path = "/etc/musallahboard/agent.key"
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServicePort(t *testing.T) {
	for extra, want := range map[string]bool{"": true, "service_port = true": true, "service_port = false": false} {
		c, err := Load(writeConfig(t, baseTOML+extra+"\n"))
		if err != nil {
			t.Fatalf("%q: %v", extra, err)
		}
		if got := c.ServicePort(); got != want {
			t.Errorf("%q: ServicePort = %v, want %v", extra, got, want)
		}
	}
}

func TestDefaults(t *testing.T) {
	c, err := Load(writeConfig(t, baseTOML))
	if err != nil {
		t.Fatal(err)
	}
	if !c.USBImport() || !c.ContentSync() || !c.AutoUpdate() || c.ContentDays() != DefaultContentDays {
		t.Errorf("defaults wrong: %+v", c)
	}
	if !strings.HasSuffix(c.AgentChannelURL("arm64"), "agent-channel-arm64.json") {
		t.Errorf("agent channel = %s", c.AgentChannelURL("arm64"))
	}
	if c, _ := Load(writeConfig(t, baseTOML+"content_days = 92\n")); c.ContentDays() != 92 {
		t.Errorf("content_days = 92 gave %d", c.ContentDays())
	}
	c, _ = Load(writeConfig(t, baseTOML+"app_channel_url = \"\"\ncontent_days = 99\n"))
	if c.AppChannelURL() != "" || c.ContentDays() != DefaultContentDays {
		t.Errorf("overrides wrong: app=%q days=%d", c.AppChannelURL(), c.ContentDays())
	}
}

func TestSetServicePortKeepsOtherFields(t *testing.T) {
	p := writeConfig(t, baseTOML+"service_port = true\n")
	if err := SetServicePort(p, false); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ServicePort() || c.DeviceID != "3f2a1b4c-0000-4000-8000-000000000001" {
		t.Errorf("after SetServicePort(false): %+v", c)
	}
	// An unprovisioned board has no config yet; the switch creates one.
	missing := filepath.Join(t.TempDir(), "missing.toml")
	if err := SetServicePort(missing, true); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(missing); err != nil || !c.ServicePort() || c.Provisioned() {
		t.Fatalf("created config: %+v, %v", c, err)
	}
}

// No agent.toml is an unprovisioned board, which still runs.
func TestLoadMissingIsUnprovisioned(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "agent.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Provisioned() || c.Online() || !c.USBImport() {
		t.Fatalf("missing config: %+v", c)
	}
	if _, err := Load(writeConfig(t, "device_id = [")); err == nil {
		t.Fatal("a corrupt config loaded")
	}
}

func TestLiveUpdateSavesAndNotifies(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.toml")
	l, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	changed := l.Changed()
	if err := l.Update(func(c *Config) error { c.DeviceID = "d"; return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("Update did not signal a change")
	}
	if l.Get().DeviceID != "d" {
		t.Fatal("Update not current")
	}
	if c, _ := Load(p); c.DeviceID != "d" {
		t.Fatal("Update not saved")
	}
	// A failed update changes nothing.
	if err := l.Update(func(c *Config) error { c.DeviceID = "e"; return errors.New("no") }); err == nil || l.Get().DeviceID != "d" {
		t.Fatalf("failed update applied: %v %q", err, l.Get().DeviceID)
	}
}

func TestCheckTransport(t *testing.T) {
	cases := []struct {
		name, backend, ws string
		insecure          bool
		ok, insecureErr   bool
	}{
		{"tls", "https://api.example.com", "wss://api.example.com/api/agent/ws", false, true, false},
		{"tls, ws port differs", "https://api.example.com", "wss://api.example.com:8443/ws", false, true, false},
		{"tls, case and trailing dot", "https://API.example.com.", "wss://api.example.com/ws", false, true, false},
		{"http backend", "http://api.example.com:8080", "wss://api.example.com/ws", false, false, true},
		{"ws socket", "https://api.example.com", "ws://api.example.com/ws", false, false, true},
		{"other ws host", "https://api.example.com", "wss://evil.example.net/ws", false, false, true},
		{"other ws host, insecure dev", "http://10.0.2.2:8080", "ws://localhost:8080/ws", true, false, true},
		{"insecure dev", "http://10.0.2.2:8080", "ws://10.0.2.2:8080/api/agent/ws", true, true, false},
		{"insecure dev with tls", "https://api.example.com", "wss://api.example.com/ws", true, true, false},
		{"ftp", "ftp://api.example.com", "wss://api.example.com/ws", true, false, false},
		{"no host", "https://", "wss://api.example.com/ws", false, false, false},
		{"credentials", "https://u:p@api.example.com", "wss://api.example.com/ws", false, false, false},
		{"query", "https://api.example.com/?x=1", "wss://api.example.com/ws", false, false, false},
		{"https socket", "https://api.example.com", "https://api.example.com/ws", false, false, false},
	}
	for _, tc := range cases {
		c := &Config{BackendURL: tc.backend, WebSocketURL: tc.ws}
		if tc.insecure {
			on := true
			c.InsecureDevSet = &on
		}
		err := c.CheckTransport()
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if got := errors.Is(err, ErrInsecureTransport); got != tc.insecureErr {
			t.Errorf("%s: errors.Is(ErrInsecureTransport) = %v, want %v (%v)", tc.name, got, tc.insecureErr, err)
		}
	}
}

// An http board must still load: it keeps showing its content and the CLI
// tools keep working. The daemon refuses the backend via CheckTransport.
func TestLoadKeepsInsecureConfigReadable(t *testing.T) {
	body := strings.NewReplacer("https://", "http://", "wss://", "ws://").Replace(baseTOML)
	c, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if c.InsecureDev() {
		t.Error("insecure_dev defaults to false")
	}
	if err := c.CheckTransport(); !errors.Is(err, ErrInsecureTransport) {
		t.Errorf("CheckTransport = %v, want ErrInsecureTransport", err)
	}

	c, err = Load(writeConfig(t, body+"insecure_dev = true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.InsecureDev() || c.CheckTransport() != nil {
		t.Errorf("insecure_dev = true: InsecureDev=%v CheckTransport=%v", c.InsecureDev(), c.CheckTransport())
	}
}

// The opt-in is written as an explicit key, and only when on.
func TestSaveWritesInsecureDevOnlyWhenSet(t *testing.T) {
	p := filepath.Join(t.TempDir(), "agent.toml")
	c := &Config{DeviceID: "d", BackendURL: "https://a", WebSocketURL: "wss://a/ws"}
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "insecure_dev") {
		t.Errorf("insecure_dev written when unset:\n%s", b)
	}
	on := true
	c.InsecureDevSet = &on
	if err := Save(p, c); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if !strings.Contains(string(b), "insecure_dev = true") {
		t.Errorf("insecure_dev missing:\n%s", b)
	}
}

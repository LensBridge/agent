package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestServicePortFromV1Mode(t *testing.T) {
	cases := []struct {
		extra string
		want  bool
	}{
		{"", false},
		{`mode = "online"`, false},
		{`mode = "offline"`, true},
		{`service_port = true`, true},
		{"mode = \"offline\"\nservice_port = false", false},
	}
	for _, tc := range cases {
		c, err := Load(writeConfig(t, baseTOML+tc.extra+"\n"))
		if err != nil {
			t.Fatalf("%q: %v", tc.extra, err)
		}
		if got := c.ServicePort(); got != tc.want {
			t.Errorf("%q: ServicePort = %v, want %v", tc.extra, got, tc.want)
		}
	}
}

func TestLoadRejectsUnknownMode(t *testing.T) {
	if _, err := Load(writeConfig(t, baseTOML+`mode = "sideways"`+"\n")); err == nil {
		t.Fatal("unknown mode accepted")
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
	c, _ = Load(writeConfig(t, baseTOML+"app_channel_url = \"\"\ncontent_days = 99\n"))
	if c.AppChannelURL() != "" || c.ContentDays() != DefaultContentDays {
		t.Errorf("overrides wrong: app=%q days=%d", c.AppChannelURL(), c.ContentDays())
	}
}

func TestSetServicePortKeepsOtherFields(t *testing.T) {
	p := writeConfig(t, baseTOML+`mode = "offline"`+"\n")
	if err := SetServicePort(p, false); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ServicePort() || c.Mode != "" || c.DeviceID != "3f2a1b4c-0000-4000-8000-000000000001" {
		t.Errorf("after SetServicePort(false): %+v", c)
	}
	if err := SetServicePort(filepath.Join(t.TempDir(), "missing.toml"), true); err == nil {
		t.Fatal("expected an error for a missing config")
	}
}

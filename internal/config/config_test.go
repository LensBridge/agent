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

func TestLoadMode(t *testing.T) {
	cases := []struct {
		name    string
		extra   string
		want    string
		wantErr string
	}{
		// Every config written before offline mode existed has no mode key.
		{name: "absent means online", extra: "", want: ModeOnline},
		{name: "explicit online", extra: `mode = "online"`, want: ModeOnline},
		{name: "offline", extra: `mode = "offline"`, want: ModeOffline},
		{name: "empty string means online", extra: `mode = ""`, want: ModeOnline},
		{name: "unknown", extra: `mode = "airplane"`, wantErr: `unknown mode "airplane"`},
		{name: "wrong case", extra: `mode = "Offline"`, wantErr: "unknown mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(writeConfig(t, baseTOML+tc.extra+"\n"))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Mode != tc.want {
				t.Errorf("Mode = %q, want %q", c.Mode, tc.want)
			}
		})
	}
}

func TestSetModeKeepsOtherFields(t *testing.T) {
	p := writeConfig(t, baseTOML)

	if err := SetMode(p, ModeOffline); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != ModeOffline {
		t.Errorf("Mode = %q, want offline", c.Mode)
	}
	if c.DeviceID != "3f2a1b4c-0000-4000-8000-000000000001" || c.BackendURL != "https://api.example.com" ||
		c.WebSocketURL != "wss://api.example.com/api/agent/ws" || c.KeyPath != "/etc/musallahboard/agent.key" {
		t.Errorf("other fields changed: %+v", c)
	}

	if err := SetMode(p, ModeOnline); err != nil {
		t.Fatal(err)
	}
	if c, _ = Load(p); c.Mode != ModeOnline {
		t.Errorf("Mode = %q, want online", c.Mode)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind: %v", err)
	}
}

func TestSetModeRejectsUnknownWithoutWriting(t *testing.T) {
	p := writeConfig(t, baseTOML)
	before, _ := os.ReadFile(p)
	if err := SetMode(p, "sideways"); err == nil {
		t.Fatal("expected an error")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Error("config was rewritten despite an invalid mode")
	}
}

func TestSetModeRequiresEnrolledConfig(t *testing.T) {
	if err := SetMode(filepath.Join(t.TempDir(), "missing.toml"), ModeOffline); err == nil {
		t.Fatal("expected an error for a missing config")
	}
}

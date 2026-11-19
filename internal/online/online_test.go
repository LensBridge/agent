package online

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/LensBridge/agent/internal/core/config"
)

func TestCheckTransport(t *testing.T) {
	on := true
	cases := []struct {
		name    string
		cfg     config.Config
		wantErr bool
		wantLog string
	}{
		{"tls", config.Config{BackendURL: "https://api.example.com", WebSocketURL: "wss://api.example.com/ws"}, false, ""},
		{"http without opt-in", config.Config{BackendURL: "http://10.0.2.2:8080", WebSocketURL: "ws://10.0.2.2:8080/ws"}, true, "REFUSING TO TALK TO THE BACKEND"},
		{"socket elsewhere", config.Config{BackendURL: "https://api.example.com", WebSocketURL: "wss://other.example.com/ws"}, true, "REFUSING TO TALK TO THE BACKEND"},
		{"insecure dev", config.Config{BackendURL: "http://10.0.2.2:8080", WebSocketURL: "ws://10.0.2.2:8080/ws", InsecureDevSet: &on}, false, "INSECURE DEVELOPMENT BOARD"},
		{"insecure dev with tls still warns", config.Config{BackendURL: "https://api.example.com", WebSocketURL: "wss://api.example.com/ws", InsecureDevSet: &on}, false, "INSECURE DEVELOPMENT BOARD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := checkTransport(slog.New(slog.NewTextHandler(&buf, nil)), &tc.cfg)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantLog == "" && buf.Len() != 0 {
				t.Errorf("unexpected log: %s", buf.String())
			}
			if tc.wantLog != "" && !strings.Contains(buf.String(), tc.wantLog) {
				t.Errorf("log %q does not say %q", buf.String(), tc.wantLog)
			}
		})
	}
}

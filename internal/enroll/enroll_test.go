package enroll

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LensBridge/agent/internal/config"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// paths returns config and key paths inside a fresh temp dir.
func paths(t *testing.T) (cfgPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "agent.toml"), filepath.Join(dir, "agent.key")
}

func TestRunEnrollsAndPersistsConfig(t *testing.T) {
	const deviceID = "6f1e1d94-1f4a-4a1e-9a1e-2f3c4d5e6a7b"

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/enroll" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		// Carries an explicit port so restorePort leaves it untouched; the
		// port-patching behaviour has its own test below.
		json.NewEncoder(w).Encode(map[string]any{
			"deviceId":     deviceID,
			"websocketUrl": "ws://board.example:8080/api/agent/ws",
		})
	}))
	defer server.Close()

	cfgPath, keyPath := paths(t)
	err := Run(context.Background(), quietLogger(), Params{
		Token:        "one-time-token",
		BackendURL:   server.URL,
		ConfigPath:   cfgPath,
		KeyPath:      keyPath,
		AgentVersion: "1.2.3",
	})
	if err != nil {
		t.Fatalf("Run() = %v, want nil", err)
	}

	// The wire field names come from the generated types; if the backend renames
	// one, regeneration changes the Go struct and this assertion catches it.
	for _, field := range []string{"token", "publicKey", "hostname", "agentVersion"} {
		if _, ok := gotBody[field]; !ok {
			t.Errorf("request body missing %q; got keys %v", field, keys(gotBody))
		}
	}
	if gotBody["token"] != "one-time-token" {
		t.Errorf("token = %v", gotBody["token"])
	}
	if gotBody["agentVersion"] != "1.2.3" {
		t.Errorf("agentVersion = %v", gotBody["agentVersion"])
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.DeviceID != deviceID {
		t.Errorf("DeviceID = %q, want %q", cfg.DeviceID, deviceID)
	}
	if cfg.WebSocketURL != "ws://board.example:8080/api/agent/ws" {
		t.Errorf("WebSocketURL = %q", cfg.WebSocketURL)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("private key not written: %v", err)
	}
}

// A rejected token must surface the server's message rather than a bare status.
// Before the client was generated this response body was untyped, so the reason
// was not reliably available to report.
func TestRunReportsRejectionMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"message": "Enrollment token is invalid, expired, or already used",
		})
	}))
	defer server.Close()

	cfgPath, keyPath := paths(t)
	err := Run(context.Background(), quietLogger(), Params{
		Token:      "stale",
		BackendURL: server.URL,
		ConfigPath: cfgPath,
		KeyPath:    keyPath,
	})
	if err == nil {
		t.Fatal("Run() = nil, want error")
	}
	if !strings.Contains(err.Error(), "invalid, expired, or already used") {
		t.Errorf("error = %q, want it to carry the server message", err)
	}
	// Nothing should be persisted for a failed enrollment.
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		t.Error("config written despite failed enrollment")
	}
	if _, statErr := os.Stat(keyPath); statErr == nil {
		t.Error("private key written despite failed enrollment")
	}
}

func TestRunRejectsIncompleteResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// deviceId present, websocketUrl missing.
		json.NewEncoder(w).Encode(map[string]any{
			"deviceId": "6f1e1d94-1f4a-4a1e-9a1e-2f3c4d5e6a7b",
		})
	}))
	defer server.Close()

	cfgPath, keyPath := paths(t)
	err := Run(context.Background(), quietLogger(), Params{
		Token:      "t",
		BackendURL: server.URL,
		ConfigPath: cfgPath,
		KeyPath:    keyPath,
	})
	if err == nil || !strings.Contains(err.Error(), "missing deviceId or websocketUrl") {
		t.Fatalf("Run() = %v, want missing-field error", err)
	}
}

func TestRestorePort(t *testing.T) {
	tests := []struct {
		name, wsURL, backendURL, want string
	}{
		{
			name:       "port restored when backend has one and ws does not",
			wsURL:      "ws://board.example/api/agent/ws",
			backendURL: "http://board.example:8080",
			want:       "ws://board.example:8080/api/agent/ws",
		},
		{
			name:       "existing ws port is left alone",
			wsURL:      "ws://board.example:9999/api/agent/ws",
			backendURL: "http://board.example:8080",
			want:       "ws://board.example:9999/api/agent/ws",
		},
		{
			name:       "nothing to restore when backend has no port",
			wsURL:      "ws://board.example/api/agent/ws",
			backendURL: "https://board.example",
			want:       "ws://board.example/api/agent/ws",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := restorePort(tc.wsURL, tc.backendURL); got != tc.want {
				t.Errorf("restorePort() = %q, want %q", got, tc.want)
			}
		})
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

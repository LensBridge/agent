package enroll

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
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
	"github.com/LensBridge/agent/internal/trust"
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

// trustPathBeside is the trust store tests use: next to the config, never
// the real /etc/musallahboard/trust.json.
func trustPathBeside(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "trust.json")
}

var contentPub = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{5}, 32)).Public().(ed25519.PublicKey)

func signingKeyJSON(pub ed25519.PublicKey) map[string]any {
	return map[string]any{"keyId": trust.KeyID(pub), "publicKey": base64.StdEncoding.EncodeToString(pub)}
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
			"deviceId":           deviceID,
			"websocketUrl":       "ws://board.example:8080/api/agent/ws",
			"contentSigningKeys": []any{signingKeyJSON(contentPub)},
		})
	}))
	defer server.Close()

	cfgPath, keyPath := paths(t)
	err := Run(context.Background(), quietLogger(), Params{
		Token:        "one-time-token",
		BackendURL:   server.URL,
		ConfigPath:   cfgPath,
		KeyPath:      keyPath,
		TrustPath:    trustPathBeside(cfgPath),
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

	// The backend's content key is pinned, as a content key only.
	ts, err := trust.Load(trustPathBeside(cfgPath))
	if err != nil {
		t.Fatalf("load trust store: %v", err)
	}
	if len(ts.Content) != 1 || ts.Content[0].KeyID != trust.KeyID(contentPub) || len(ts.Release) != 0 {
		t.Errorf("trust store = %+v, want exactly the backend's content key", ts)
	}
}

// A backend with no content signing key configured sends no keys; the board
// still enrolls, and gets them later from `trust fetch`.
func TestRunWithoutContentKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"deviceId":     "6f1e1d94-1f4a-4a1e-9a1e-2f3c4d5e6a7b",
			"websocketUrl": "ws://board.example:8080/api/agent/ws",
		})
	}))
	defer server.Close()

	cfgPath, keyPath := paths(t)
	if err := Run(context.Background(), quietLogger(), Params{
		Token: "t", BackendURL: server.URL, ConfigPath: cfgPath, KeyPath: keyPath,
		TrustPath: trustPathBeside(cfgPath),
	}); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if _, err := os.Stat(trustPathBeside(cfgPath)); err == nil {
		t.Error("trust store written with no keys to pin")
	}
}

func TestPinContentKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "trust.json")
	k := SigningKey{KeyID: trust.KeyID(contentPub), PublicKey: base64.StdEncoding.EncodeToString(contentPub)}
	added, err := PinContentKeys(p, []SigningKey{k}, "lensbridge")
	if err != nil || len(added) != 1 {
		t.Fatalf("first pin: %v, %v", added, err)
	}
	// Idempotent.
	if added, err := PinContentKeys(p, []SigningKey{k}, "lensbridge"); err != nil || len(added) != 0 {
		t.Fatalf("second pin: %v, %v", added, err)
	}
	// A key whose id does not match is refused, and nothing else in the
	// same response is taken either.
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{6}, 32)).Public().(ed25519.PublicKey)
	bad := SigningKey{KeyID: "0000000000000000", PublicKey: base64.StdEncoding.EncodeToString(other)}
	good := SigningKey{PublicKey: base64.StdEncoding.EncodeToString(other)}
	if _, err := PinContentKeys(p, []SigningKey{good, bad}, "x"); err == nil {
		t.Fatal("mismatched key id accepted")
	}
	ts, _ := trust.Load(p)
	if len(ts.Content) != 1 {
		t.Fatalf("trust store changed by a refused response: %+v", ts)
	}
}

func TestFetchSigningKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/signing-keys" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"content": []any{signingKeyJSON(contentPub)}})
	}))
	defer server.Close()
	keys, err := FetchSigningKeys(context.Background(), server.URL+"/", nil)
	if err != nil || len(keys) != 1 || keys[0].KeyID != trust.KeyID(contentPub) {
		t.Fatalf("FetchSigningKeys = %v, %v", keys, err)
	}
	if _, err := FetchSigningKeys(context.Background(), server.URL+"/nope", nil); err == nil {
		t.Fatal("404 not reported")
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
		TrustPath:  trustPathBeside(cfgPath),
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
		TrustPath:  trustPathBeside(cfgPath),
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

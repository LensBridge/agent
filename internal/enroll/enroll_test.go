package enroll

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// wsURL is the WebSocket URL a backend at server would hand out.
func wsURL(server *httptest.Server, scheme string) string {
	return scheme + "://" + strings.TrimPrefix(strings.TrimPrefix(server.URL, "https://"), "http://") + "/api/agent/ws"
}

func TestRunEnrollsAndPersistsConfig(t *testing.T) {
	const deviceID = "6f1e1d94-1f4a-4a1e-9a1e-2f3c4d5e6a7b"

	var gotBody map[string]any
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			"websocketUrl":       wsURL(server, "wss"),
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
		HTTPClient:   server.Client(),
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
	if cfg.WebSocketURL != wsURL(server, "wss") {
		t.Errorf("WebSocketURL = %q", cfg.WebSocketURL)
	}
	if cfg.InsecureDev() || cfg.CheckTransport() != nil {
		t.Errorf("a TLS enrollment must give a secure config: insecure_dev=%v, CheckTransport=%v", cfg.InsecureDev(), cfg.CheckTransport())
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
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"deviceId":     "6f1e1d94-1f4a-4a1e-9a1e-2f3c4d5e6a7b",
			"websocketUrl": wsURL(server, "wss"),
		})
	}))
	defer server.Close()

	cfgPath, keyPath := paths(t)
	if err := Run(context.Background(), quietLogger(), Params{
		Token: "t", BackendURL: server.URL, ConfigPath: cfgPath, KeyPath: keyPath,
		TrustPath: trustPathBeside(cfgPath), HTTPClient: server.Client(),
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
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		HTTPClient: server.Client(),
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
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		HTTPClient: server.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "missing deviceId or websocketUrl") {
		t.Fatalf("Run() = %v, want missing-field error", err)
	}
}

// A plain-http backend is refused before anything is sent: the reply would
// pin the content keys and the WebSocket URL, and anyone on the LAN could
// write it.
func TestRunRefusesPlainHTTPBackend(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer server.Close()

	cfgPath, keyPath := paths(t)
	err := Run(context.Background(), quietLogger(), Params{
		Token: "t", BackendURL: server.URL, ConfigPath: cfgPath, KeyPath: keyPath,
		TrustPath: trustPathBeside(cfgPath),
	})
	if !errors.Is(err, config.ErrInsecureTransport) || !strings.Contains(err.Error(), "--insecure-dev") {
		t.Fatalf("Run() = %v, want ErrInsecureTransport naming --insecure-dev", err)
	}
	if hits != 0 {
		t.Errorf("the token was sent over plain http (%d requests)", hits)
	}
	assertNothingSaved(t, cfgPath, keyPath)
}

// --insecure-dev enrolls over http and records the opt-in in the config.
func TestRunInsecureDevEnrollsOverHTTP(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"deviceId":     "6f1e1d94-1f4a-4a1e-9a1e-2f3c4d5e6a7b",
			"websocketUrl": wsURL(server, "ws"),
		})
	}))
	defer server.Close()

	cfgPath, keyPath := paths(t)
	if err := Run(context.Background(), quietLogger(), Params{
		Token: "t", BackendURL: server.URL, ConfigPath: cfgPath, KeyPath: keyPath,
		TrustPath: trustPathBeside(cfgPath), InsecureDev: true,
	}); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	b, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(b), "insecure_dev = true") {
		t.Errorf("insecure_dev not written:\n%s", b)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil || cfg.CheckTransport() != nil {
		t.Fatalf("Load = %v, CheckTransport = %v", err, cfg.CheckTransport())
	}
}

// The backend's WebSocket URL must be wss:// on the backend's own host;
// otherwise nothing is saved and the board stays unenrolled.
func TestRunRefusesUnsafeWebSocketURL(t *testing.T) {
	for name, ws := range map[string]func(*httptest.Server) string{
		"plain ws":   func(s *httptest.Server) string { return wsURL(s, "ws") },
		"other host": func(*httptest.Server) string { return "wss://attacker.example/api/agent/ws" },
	} {
		t.Run(name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"deviceId":           "6f1e1d94-1f4a-4a1e-9a1e-2f3c4d5e6a7b",
					"websocketUrl":       ws(server),
					"contentSigningKeys": []any{signingKeyJSON(contentPub)},
				})
			}))
			defer server.Close()

			cfgPath, keyPath := paths(t)
			err := Run(context.Background(), quietLogger(), Params{
				Token: "t", BackendURL: server.URL, ConfigPath: cfgPath, KeyPath: keyPath,
				TrustPath: trustPathBeside(cfgPath), HTTPClient: server.Client(),
			})
			if !errors.Is(err, config.ErrInsecureTransport) {
				t.Fatalf("Run() = %v, want ErrInsecureTransport", err)
			}
			assertNothingSaved(t, cfgPath, keyPath)
			if _, err := os.Stat(trustPathBeside(cfgPath)); err == nil {
				t.Error("content keys pinned from a refused reply")
			}
		})
	}
}

func assertNothingSaved(t *testing.T, cfgPath, keyPath string) {
	t.Helper()
	if _, err := os.Stat(cfgPath); err == nil {
		t.Error("config written")
	}
	if _, err := os.Stat(keyPath); err == nil {
		t.Error("private key written")
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

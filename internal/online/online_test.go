package online

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/core/config"
	"github.com/LensBridge/agent/internal/core/kiosk"
	"github.com/LensBridge/agent/internal/core/store"
	"github.com/LensBridge/agent/internal/online/wsclient"
)

const testDevice = "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b"

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

// A board set up with a backend claims its key (retrying while the backend
// is not answering, with the same key), saves the WebSocket URL, drops the
// claim secret, and the layer starts over online.
func TestLayerClaims(t *testing.T) {
	var claims []map[string]string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/claim" {
			http.NotFound(w, r)
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		claims = append(claims, body)
		n := len(claims)
		mu.Unlock()
		if n == 1 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"deviceId": testDevice,
			"websocketUrl": "ws://" + r.Host + "/api/agent/ws", "contentSigningKeys": []any{}})
	}))
	defer srv.Close()

	dir := t.TempDir()
	on := true
	path := filepath.Join(dir, "agent.toml")
	secret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	if err := config.Save(path, &config.Config{DeviceID: testDevice, BackendURL: srv.URL, ClaimSecret: secret, InsecureDevSet: &on}); err != nil {
		t.Fatal(err)
	}
	live, _ := config.Open(path)
	l := New(Deps{Config: live, KeyPath: filepath.Join(dir, "agent.key"), TrustPath: filepath.Join(dir, "trust.json"),
		Layout: store.Layout{Root: dir}, Kiosk: kiosk.NewClient("http://127.0.0.1:1"),
		BoardReport: func() any { return nil }, AgentVersion: "0.4.0", HTTP: srv.Client(),
		ClaimRetry: 10 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	for live.Get().WebSocketURL == "" && ctx.Err() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	// The layer starts over with the saved URL: now it is "connecting".
	for {
		if _, ok := l.Backend().(*wsclient.ConnState); ok {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("layer did not start online: %#v", l.Backend())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	saved, _ := config.Load(path)
	if saved.WebSocketURL == "" || saved.ClaimSecret != "" {
		t.Fatalf("saved config = %+v", saved)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(claims) < 2 || claims[0]["publicKey"] != claims[1]["publicKey"] {
		t.Fatalf("claims = %v", claims)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.key")); err != nil {
		t.Fatal("device key not saved")
	}
}

// An offline board (no backend) has no online layer, and says nothing.
func TestLayerOffline(t *testing.T) {
	l := New(Deps{Config: config.InMemory(config.Config{DeviceID: testDevice})})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	l.Run(ctx)
	if l.Backend() != nil {
		t.Fatalf("backend = %v", l.Backend())
	}
}

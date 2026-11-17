package wsclient

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LensBridge/agent/internal/config"
)

// A plaintext or foreign-host WebSocket URL is never dialled without the
// insecure_dev opt-in.
func TestRunRefusesInsecureWebSocketURL(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	for name, cfg := range map[string]*config.Config{
		"ws":         {DeviceID: "d", BackendURL: srv.URL, WebSocketURL: "ws://" + host + "/ws"},
		"other host": {DeviceID: "d", BackendURL: "https://api.example.com", WebSocketURL: "wss://" + host + "/ws"},
	} {
		t.Run(name, func(t *testing.T) {
			c := New(cfg, nil, slog.New(slog.DiscardHandler), "test", false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c.Run(ctx) // must return on its own
			if ctx.Err() != nil {
				t.Fatal("Run kept going")
			}
			if hits.Load() != 0 {
				t.Errorf("dialled %d times", hits.Load())
			}
			st := c.State()
			if st.Connected || st.LastError == "" {
				t.Errorf("state = %+v, want the reason in lastError", st)
			}
			if !errors.Is(cfg.CheckTransport(), config.ErrInsecureTransport) {
				t.Errorf("CheckTransport = %v", cfg.CheckTransport())
			}
		})
	}
}

package boardsync

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// refreshStable is how long a refresh connection must stay up before the
// reconnect backoff starts over. A server that accepts and immediately drops
// the socket would otherwise be redialled every second.
const refreshStable = 30 * time.Second

// refreshLoop holds the backend's refresh channel open, the same socket the
// hosted board listens on. The backend sends a message whenever something on
// the board's content changes; each one schedules a content sync.
func (s *Syncer) refreshLoop(ctx context.Context) {
	wsURL, err := refreshURL(s.d.Cfg.BackendURL, s.d.Cfg.DeviceID)
	if err != nil {
		s.log.Warn("no refresh channel: cannot derive its address from backend_url", "err", err)
		return
	}
	backoff := s.refreshMinBackoff
	for {
		started := time.Now()
		err := s.refreshOnce(ctx, wsURL)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= refreshStable {
			backoff = s.refreshMinBackoff
		}
		s.log.Debug("refresh channel closed", "err", err, "retryIn", backoff)
		if !sleep(ctx, backoff+jitter(backoff)) {
			return
		}
		backoff = min(backoff*2, s.refreshMaxBackoff)
	}
}

func (s *Syncer) refreshOnce(ctx context.Context, wsURL string) error {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// The shared client's overall timeout is for downloads; the socket is
	// meant to stay open for hours and is bounded by dialCtx instead.
	wsClient := *s.client
	wsClient.Timeout = 0
	conn, _, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPClient: &wsClient,
		HTTPHeader: http.Header{"User-Agent": []string{"musallahboard-agent/" + s.d.AgentVersion}},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 10)
	s.log.Debug("refresh channel connected")
	for {
		typ, _, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ == websocket.MessageText {
			s.Trigger()
		}
	}
}

// refreshURL derives ws(s)://<backend>/api/refresh-musallahboard?deviceId=<id>
// from backend_url.
func refreshURL(backendURL, deviceID string) (string, error) {
	u, err := url.Parse(strings.TrimRight(backendURL, "/"))
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("backend_url scheme %q is not http or https", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/api/refresh-musallahboard"
	u.RawPath = ""
	u.RawQuery = url.Values{"deviceId": {deviceID}}.Encode()
	u.Fragment = ""
	return u.String(), nil
}

func jitter(b time.Duration) time.Duration {
	if b < 4 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(b / 4)))
}

func removeAll(p string) { _ = os.RemoveAll(p) }

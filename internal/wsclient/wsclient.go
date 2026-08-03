// Package wsclient owns the persistent agent ↔ backend WebSocket connection.
//
// One instance of Client supervises the whole lifetime: dial → handshake →
// heartbeat loop → command dispatch → reconnect on close. Run blocks until
// the context is cancelled.
//
// The handshake matches the backend's AuthSignaturePayload spec:
// hello (server) → auth (client, signs challenge) → auth_ok (server). On any
// failure we close, sleep with jittered exponential backoff (1 s → 5 min),
// and dial again.
package wsclient

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/utmmsa/musallahboard-agent/internal/config"
	"github.com/utmmsa/musallahboard-agent/internal/telemetry"
)

const (
	defaultHeartbeatInterval = 30 * time.Second
	dialTimeout              = 15 * time.Second
	authTimeout              = 10 * time.Second
	writeTimeout             = 10 * time.Second
	maxFrameBytes = 20 << 20 // 20 MiB	
	minBackoff               = time.Second
	maxBackoff               = 5 * time.Minute

	// authSkewLimit mirrors AgentWebSocketHandler.TIMESTAMP_SKEW_MS. Past this
	// the backend refuses the handshake outright.
	authSkewLimit = 5 * time.Minute
	// clockSkewWarn is well inside that limit, so a drifting clock is visible in
	// the journal before it starts costing sessions.
	clockSkewWarn = 30 * time.Second
)

// Command is the parsed view of an inbound `command` frame, handed to the
// L6c-registered handler. Payload stays as raw JSON so the handler can decode
// it into its kind-specific record.
type Command struct {
	ID         string
	Kind       string
	IssuedBy   string
	DeadlineMs int
	Payload    json.RawMessage
}

// CommandHandler is the L6c integration point. It must return promptly; long-
// running work belongs in a goroutine the handler spawns.
type CommandHandler func(ctx context.Context, sender Sender, cmd Command)

// Client is the long-lived WS supervisor. Construct with New, register a
// command handler with SetCommandHandler, then call Run.
type Client struct {
	cfg          *config.Config
	priv         ed25519.PrivateKey
	logger       *slog.Logger
	agentVersion string
	safeMode     bool

	onCommand CommandHandler
	prober    telemetry.PageProber
}

func New(cfg *config.Config, priv ed25519.PrivateKey, logger *slog.Logger, agentVersion string, safeMode bool) *Client {
	return &Client{
		cfg:          cfg,
		priv:         priv,
		logger:       logger,
		agentVersion: agentVersion,
		safeMode:     safeMode,
	}
}

// SetCommandHandler registers the callback fired for every `command` frame.
// Must be called before Run; after Run starts, the field is read-only.
func (c *Client) SetCommandHandler(h CommandHandler) { c.onCommand = h }

// SetPageProber supplies the browser probe used to fill kioskAlive and
// displayedFrameKey. Optional — without one, heartbeats fall back to the
// systemd unit state. Must be called before Run.
func (c *Client) SetPageProber(p telemetry.PageProber) { c.prober = p }

// Run blocks until ctx is cancelled, repeatedly attempting to maintain a live
// authenticated session. Errors are logged and trigger a backoff-and-retry.
func (c *Client) Run(ctx context.Context) {
	backoff := minBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		authed, err := c.runOnce(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			c.logger.Warn("ws session ended", "err", err, "backoff", backoff)
		}

		// A session that got as far as auth proves the backend is reachable and
		// our credentials are good, so the next drop starts over at one second.
		// Without this the delay only ever climbs: one bad frame mid-session and
		// a healthy device settles into reconnecting every five minutes.
		if authed {
			backoff = minBackoff
		}

		if !sleep(ctx, backoff+jitter(backoff)) {
			return
		}
		if !authed {
			backoff = nextBackoff(backoff)
		}
	}
}

// runOnce executes a single dial → auth → serve cycle. Returns when the session
// ends for any reason, and whether it got as far as a completed handshake.
func (c *Client) runOnce(ctx context.Context) (authed bool, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, c.cfg.WebSocketURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"User-Agent": []string{"musallahboard-agent/" + c.agentVersion}},
	})
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	conn.SetReadLimit(maxFrameBytes)
	defer conn.CloseNow()

	c.logger.Info("ws connected", "url", c.cfg.WebSocketURL)

	authCtx, cancelAuth := context.WithTimeout(ctx, authTimeout)
	defer cancelAuth()

	hello, err := readFrame(authCtx, conn)
	if err != nil {
		return false, fmt.Errorf("read hello: %w", err)
	}
	if hello.Type != "hello" {
		return false, fmt.Errorf("expected hello, got %q", hello.Type)
	}
	if hello.SessionID == "" || hello.Challenge == "" {
		return false, errors.New("hello frame missing sessionId or challenge")
	}

	sess := newSession(conn, hello.SessionID, c.cfg.DeviceID)

	now := time.Now().UnixMilli()
	c.warnOnClockSkew(hello.ServerTime, now)
	auth := AuthFrame{
		Type:      "auth",
		Seq:       sess.nextSeq(),
		SessionID: hello.SessionID,
		DeviceID:  c.cfg.DeviceID,
		Timestamp: now,
		Signature: SignAuth(c.priv, hello.SessionID, hello.Challenge, c.cfg.DeviceID, now),
	}
	if err := sess.sendJSON(authCtx, auth); err != nil {
		return false, fmt.Errorf("send auth: %w", err)
	}

	authOk, err := readFrame(authCtx, conn)
	if err != nil {
		return false, fmt.Errorf("read auth_ok: %w", err)
	}
	if authOk.Type != "auth_ok" {
		return false, fmt.Errorf("expected auth_ok, got %q", authOk.Type)
	}

	hbInterval := defaultHeartbeatInterval
	if authOk.HeartbeatIntervalMs > 0 {
		hbInterval = time.Duration(authOk.HeartbeatIntervalMs) * time.Millisecond
	}
	c.logger.Info("ws authenticated",
		"sessionId", hello.SessionID,
		"deviceId", authOk.DeviceID,
		"heartbeatInterval", hbInterval,
	)

	return true, c.serve(ctx, sess, hbInterval)
}

// warnOnClockSkew compares our clock against the server's and says so plainly
// when they disagree.
//
// The backend rejects an auth signature whose timestamp is more than
// authSkewLimit out, and a Pi has no battery-backed RTC — after a power cut it
// boots with whatever time it last knew until NTP catches up. The failure then
// looks like "auth_failed" forever with nothing in the journal explaining why,
// so name the real cause. Only the log can help here: re-signing with the
// server's clock would defeat the replay protection the timestamp exists for.
func (c *Client) warnOnClockSkew(serverTimeMs, localTimeMs int64) {
	if serverTimeMs == 0 {
		return // older backend, or a hello without serverTime
	}
	skew := time.Duration(localTimeMs-serverTimeMs) * time.Millisecond
	if skew < 0 {
		skew = -skew
	}
	if skew < clockSkewWarn {
		return
	}
	c.logger.Error("system clock disagrees with the backend — authentication will fail past the limit; check NTP (timedatectl)",
		"skew", skew.Round(time.Second),
		"limit", authSkewLimit,
	)
}

// serve drives the read + heartbeat loops until either ctx fires, the
// connection closes, or a fatal frame is received. Returns the cause.
func (c *Client) serve(ctx context.Context, sess *session, hbInterval time.Duration) error {
	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)

	go func() { errCh <- c.heartbeatLoop(sessCtx, sess, hbInterval) }()
	go func() { errCh <- c.readLoop(sessCtx, sess) }()

	err := <-errCh
	cancel()
	// drain the second goroutine so it can't outlive the session.
	<-errCh
	return err
}

func (c *Client) heartbeatLoop(ctx context.Context, sess *session, interval time.Duration) error {
	send := func() error {
		snap := telemetry.Collect(ctx, c.agentVersion, c.safeMode, c.prober)
		hb := HeartbeatFrame{
			Type:      "heartbeat",
			Seq:       sess.nextSeq(),
			SessionID: sess.sessionID,
			Telemetry: TelemetryFromSnapshot(snap),
		}
		wctx, cancel := context.WithTimeout(ctx, writeTimeout)
		defer cancel()
		return sess.sendJSON(wctx, hb)
	}

	if err := send(); err != nil {
		return fmt.Errorf("first heartbeat: %w", err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := send(); err != nil {
				return fmt.Errorf("heartbeat: %w", err)
			}
		}
	}
}

func (c *Client) readLoop(ctx context.Context, sess *session) error {
	for {
		f, err := readFrame(ctx, sess.conn)
		if err != nil {
			return err
		}
		switch f.Type {
		case "command":
			if c.onCommand == nil {
				c.logger.Warn("dropping command — no handler registered", "commandId", f.CommandID, "kind", f.Kind)
				continue
			}
			cmd := Command{
				ID:         f.CommandID,
				Kind:       f.Kind,
				IssuedBy:   f.IssuedBy,
				DeadlineMs: f.DeadlineMs,
				Payload:    f.Payload,
			}
			// Handlers run in a goroutine so a slow command doesn't block heartbeats.
			go c.onCommand(ctx, sessionSender{s: sess}, cmd)
		default:
			c.logger.Debug("ignoring frame", "type", f.Type)
		}
	}
}

// readFrame reads the next text message and decodes it as an IncomingFrame.
// EOF and normal closures are returned verbatim — the caller decides whether
// the close is expected.
func readFrame(ctx context.Context, conn *websocket.Conn) (*IncomingFrame, error) {
	mt, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if mt != websocket.MessageText {
		return nil, fmt.Errorf("non-text frame (kind=%v)", mt)
	}
	if len(data) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	var f IncomingFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	return &f, nil
}

func nextBackoff(b time.Duration) time.Duration {
	n := b * 2
	if n > maxBackoff {
		return maxBackoff
	}
	return n
}

func jitter(b time.Duration) time.Duration {
	if b <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(b / 4)))
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

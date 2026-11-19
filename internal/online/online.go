// Package online is the board's optional fleet layer (docs/architecture.md,
// section 9): content sync and weather from the LensBridge backend, and the
// backend WebSocket with telemetry and remote commands.
//
// It runs only on a board whose agent.toml names a device and a backend, and
// whose device key is bound to it (by `enroll`, or by the claim a setup file
// starts). Everything it does goes through the core: packages through the
// importer, the status through the local server. Nothing in the core depends
// on it, and a board without it loses no board function.
package online

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/LensBridge/agent/internal/core/config"
	"github.com/LensBridge/agent/internal/core/importer"
	"github.com/LensBridge/agent/internal/core/kiosk"
	"github.com/LensBridge/agent/internal/core/store"
	"github.com/LensBridge/agent/internal/core/trust"
	"github.com/LensBridge/agent/internal/core/updates"
	"github.com/LensBridge/agent/internal/online/api"
	"github.com/LensBridge/agent/internal/online/boardsync"
	"github.com/LensBridge/agent/internal/online/commands"
	"github.com/LensBridge/agent/internal/online/enroll"
	"github.com/LensBridge/agent/internal/online/keystore"
	"github.com/LensBridge/agent/internal/online/wsclient"
)

// Deps is what the online layer needs from the board.
type Deps struct {
	Config *config.Live
	// KeyPath is the device key; config.KeyPath on a board.
	KeyPath  string
	Layout   store.Layout
	Importer *importer.Importer
	Updates  *updates.Scheduler
	Kiosk    *kiosk.Client
	// BoardReport is what every heartbeat says about the board.
	BoardReport  func() any
	AgentVersion string
	Logger       *slog.Logger
	// TrustPath is trust.json, to say whether the backend's content keys
	// are pinned; trust.DefaultPath on a board.
	TrustPath string
	// HTTP is optional (tests).
	HTTP *http.Client
	// ClaimRetry is the first wait after a failed claim; zero is
	// ClaimRetry (tests shorten it).
	ClaimRetry time.Duration
}

// Claim retry: a board set up for a backend it cannot reach yet (no network
// until someone plugs one in) keeps trying, without ever blocking the board.
const (
	ClaimRetry    = time.Minute
	ClaimRetryMax = time.Hour
)

// Layer runs the online layer for whatever agent.toml says, and reports its
// state to the local status.
type Layer struct {
	d Deps

	mu     sync.Mutex
	syncer *boardsync.Syncer
	ws     *wsclient.Client
	// why is the reason the layer is configured but not running, or "".
	why string
	// on is whether agent.toml asks for the layer at all.
	on, contentSync bool
}

// New returns a Layer. Call Run to start it.
func New(d Deps) *Layer {
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.KeyPath == "" {
		d.KeyPath = config.KeyPath
	}
	if d.TrustPath == "" {
		d.TrustPath = trust.DefaultPath
	}
	if d.ClaimRetry == 0 {
		d.ClaimRetry = ClaimRetry
	}
	return &Layer{d: d}
}

// settings is the part of agent.toml the online layer is built from; a
// change to it restarts the layer.
type settings struct {
	deviceID, backendURL, websocketURL, claimSecret string
	insecureDev, contentSync                        bool
}

func settingsOf(c *config.Config) settings {
	return settings{c.DeviceID, c.BackendURL, c.WebSocketURL, c.ClaimSecret, c.InsecureDev(), c.ContentSync()}
}

// Run runs the layer until ctx ends, starting it over whenever its part of
// agent.toml changes (a setup file applied, a claim completed).
func (l *Layer) Run(ctx context.Context) {
	for {
		changed := l.d.Config.Changed()
		cfg := l.d.Config.Get()
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			l.runOnce(runCtx, cfg)
		}()
		restart := false
		for !restart {
			select {
			case <-ctx.Done():
				cancel()
				<-done
				return
			case <-changed:
				changed = l.d.Config.Changed()
				restart = settingsOf(l.d.Config.Get()) != settingsOf(cfg)
			}
		}
		l.d.Logger.Info("online settings changed; restarting the online layer")
		cancel()
		<-done
	}
}

// runOnce runs the layer for cfg until ctx ends or it cannot run.
func (l *Layer) runOnce(ctx context.Context, cfg *config.Config) {
	l.set(nil, nil, cfg.Online(), cfg.ContentSync(), "")
	if !cfg.Online() {
		return
	}
	if cfg.WebSocketURL == "" {
		if cfg.ClaimSecret == "" {
			l.set(nil, nil, true, cfg.ContentSync(), "not enrolled: no device key is bound to this board yet")
			return
		}
		// Once the claim is saved, Run starts the layer over with it.
		l.claim(ctx, cfg)
		return
	}
	priv, err := keystore.Load(l.d.KeyPath)
	if err != nil {
		l.d.Logger.Error("device key unreadable: no sync, no remote commands", "err", err, "keyPath", l.d.KeyPath)
		l.set(nil, nil, true, cfg.ContentSync(), "device key unreadable")
		return
	}
	if err := checkTransport(l.d.Logger, cfg); err != nil {
		l.set(nil, nil, true, cfg.ContentSync(), "not talking to LensBridge: "+err.Error())
		return
	}
	l.serve(ctx, cfg, priv)
}

// claim binds the device key (made here if the board has none; it never
// leaves the board) to the device named in agent.toml, with the setup file's
// claim secret, retrying with backoff until it works or ctx ends. On success
// it saves the WebSocket URL and drops the secret.
func (l *Layer) claim(ctx context.Context, cfg *config.Config) {
	log := l.d.Logger.With("backendUrl", cfg.BackendURL, "deviceId", cfg.DeviceID)
	why := func(s string) { l.set(nil, nil, true, cfg.ContentSync(), s) }
	// The reply decides where the board takes commands from, so it must
	// come over TLS.
	if err := config.CheckBackendURL(cfg.BackendURL, cfg.InsecureDev()); err != nil {
		log.Error("not claiming: the setup file's backend is not safe to use", "err", err)
		why("not talking to LensBridge: " + err.Error())
		return
	}
	priv, err := deviceKey(l.d.KeyPath)
	if err != nil {
		log.Error("no device key: cannot claim", "err", err, "keyPath", l.d.KeyPath)
		why("device key unreadable")
		return
	}
	wait := l.d.ClaimRetry
	why("claiming this board at " + cfg.BackendURL)
	for {
		err := l.claimOnce(ctx, cfg, priv)
		if err == nil || ctx.Err() != nil {
			return
		}
		if errors.Is(err, enroll.ErrKeyConflict) {
			log.Error("claim refused: LensBridge has a different key for this board. Reset it in the console and use a new setup file", "err", err)
			wait = ClaimRetryMax
		} else {
			log.Warn("claim failed; retrying", "err", err, "retryIn", wait)
		}
		why("claim failed: " + err.Error())
		t := time.NewTimer(wait + time.Duration(rand.Int63n(int64(wait/4)+1)))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = min(wait*2, ClaimRetryMax)
	}
}

func (l *Layer) claimOnce(ctx context.Context, cfg *config.Config, priv ed25519.PrivateKey) error {
	r, err := enroll.Claim(ctx, enroll.ClaimParams{
		BackendURL: cfg.BackendURL, DeviceID: cfg.DeviceID, ClaimSecret: cfg.ClaimSecret,
		Key: priv, AgentVersion: l.d.AgentVersion, HTTP: l.d.HTTP,
	})
	if err != nil {
		return err
	}
	ws := *r.WebsocketUrl
	if err := config.CheckWebSocketURL(ws, cfg.BackendURL, cfg.InsecureDev()); err != nil {
		return fmt.Errorf("the backend's reply cannot be used: %w", err)
	}
	l.checkPinned(r.ContentSigningKeys)
	err = l.d.Config.Update(func(c *config.Config) error {
		if settingsOf(c) != settingsOf(cfg) {
			return errors.New("agent.toml changed during the claim")
		}
		c.WebSocketURL, c.ClaimSecret = ws, ""
		return nil
	})
	if err != nil {
		return err
	}
	l.d.Logger.Info("board claimed: its device key is bound in LensBridge", "deviceId", cfg.DeviceID, "websocketUrl", ws)
	return nil
}

// checkPinned says when the backend signs content with a key this board does
// not trust. The daemon cannot pin keys (trust.json is root's); setup.sh
// pins the content key, and `trust fetch` re-reads the backend's.
func (l *Layer) checkPinned(keys []api.SigningKeyView) {
	ring, err := trust.LoadRing(l.d.TrustPath)
	if err != nil {
		return
	}
	pinned := ring.Keys(trust.RoleContent)
	for _, k := range keys {
		if _, ok := pinned[k.KeyId]; !ok {
			l.d.Logger.Warn("LensBridge signs content with a key this board does not trust; its content will be refused until it is pinned: sudo musallahboard-agent trust fetch",
				"keyId", k.KeyId)
		}
	}
}

// deviceKey loads the device key, or makes and saves one if there is none.
// It is saved before it is first sent, so every retry claims the same key.
func deviceKey(path string) (ed25519.PrivateKey, error) {
	priv, err := keystore.Load(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return priv, err
	}
	_, priv, err = keystore.Generate()
	if err != nil {
		return nil, err
	}
	if err := keystore.Save(path, priv); err != nil {
		return nil, err
	}
	return priv, nil
}

// serve runs content sync and the backend WebSocket until ctx ends.
func (l *Layer) serve(ctx context.Context, cfg *config.Config, priv []byte) {
	log := l.d.Logger
	syncer := boardsync.New(boardsync.Deps{
		Cfg: cfg, Key: priv, Layout: l.d.Layout, Importer: l.d.Importer,
		AgentVersion: l.d.AgentVersion, Logger: log, HTTP: l.d.HTTP,
	})
	ws := wsclient.New(cfg, priv, log, l.d.AgentVersion)
	ws.SetPageProber(l.d.Kiosk)
	ws.SetBoardReport(l.d.BoardReport)
	ws.SetContentChanged(syncer.Trigger)
	registry := commands.NewRegistry()
	registry.Register(&commands.ChromeReload{CDP: l.d.Kiosk})
	registry.Register(&commands.ChromeScreenshot{CDP: l.d.Kiosk})
	registry.Register(&commands.ConfigRefresh{CDP: l.d.Kiosk, Before: syncer.Trigger})
	registry.Register(&commands.KioskRestart{})
	registry.Register(&commands.SystemReboot{})
	registry.Register(&commands.LogsTail{})
	registry.Register(&commands.UpdateInstallNow{Run: l.d.Updates.CheckAndInstall})
	log.Info("command handlers registered", "kinds", registry.Kinds())
	ws.SetCommandHandler(commands.NewDispatcher(registry, log).Handle)

	l.set(syncer, ws, true, cfg.ContentSync(), "")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); syncer.Run(ctx) }()
	go func() { defer wg.Done(); ws.Run(ctx) }()
	wg.Wait()
	l.set(nil, nil, true, cfg.ContentSync(), "")
}

func (l *Layer) set(s *boardsync.Syncer, ws *wsclient.Client, on, contentSync bool, why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.syncer, l.ws, l.on, l.contentSync, l.why = s, ws, on, contentSync, why
}

// Sync is the "sync" object of /api/local/status.
func (l *Layer) Sync() any {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.syncer != nil:
		return l.syncer.Status()
	case l.why != "":
		// Enabled as configured, so the board app and `status` show the
		// reason as a sync error rather than "sync off".
		return map[string]any{"enabled": l.contentSync, "lastError": l.why}
	}
	return boardsync.Status{Enabled: false}
}

// Weather is the live weather overlay, if there is one.
func (l *Layer) Weather() (json.RawMessage, bool) {
	l.mu.Lock()
	s := l.syncer
	l.mu.Unlock()
	if s == nil {
		return nil, false
	}
	return s.Weather()
}

// Backend is the "backend" object of /api/local/status: the WebSocket's
// state, why there is none, or nil on a board without the online layer.
func (l *Layer) Backend() any {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.ws != nil:
		s := l.ws.State()
		return &s
	case l.why != "":
		return map[string]any{"connected": false, "lastError": l.why}
	}
	return nil
}

// checkTransport checks that the config may be used to talk to the backend
// (config.CheckTransport) and says so loudly at every start when it may not,
// or when it may only because of the insecure_dev opt-in. A board enrolled
// over plain http before https was required lands here: it keeps showing what
// it has, and the log, `musallahboard-agent status` and the board app's sync
// error all carry the reason and the fix.
func checkTransport(logger *slog.Logger, cfg *config.Config) error {
	if err := cfg.CheckTransport(); err != nil {
		logger.Error("REFUSING TO TALK TO THE BACKEND: this board's config is not safe to use. "+
			"Serving installed content only: no content sync, no remote commands. "+
			"Re-enroll it over https (sudo musallahboard-agent enroll --force --token=... --backend=https://...), "+
			"or, for a development board only, add insecure_dev = true to "+config.DefaultPath+" and restart the agent.",
			"err", err, "backendUrl", cfg.BackendURL, "websocketUrl", cfg.WebSocketURL)
		return err
	}
	if cfg.InsecureDev() {
		logger.Warn("INSECURE DEVELOPMENT BOARD: insecure_dev = true, so this board accepts a plaintext backend. "+
			"Anyone on its network can impersonate the backend: send it content, reboot it, read its screen and logs. "+
			"Never deploy a board like this.",
			"backendUrl", cfg.BackendURL, "websocketUrl", cfg.WebSocketURL)
	}
	return nil
}
